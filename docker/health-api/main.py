"""Health Connect sync endpoint for the health-check bot.

Accepts POSTs from the Agento Android app and persists to the
shared remote MongoDB collection the health-check bot also reads
(hc_days — one doc per date, same shape the MCP writes).

Auth: single password via `Authorization: Bearer <token>`.
PASSWORD is the one credential for chat + sync (matches the
Password field in the Agento app Settings).

Env:
  MONGODB_URI  connection string (required)
  MONGODB_DB   database name (default: hermes)
"""

from __future__ import annotations

import logging
import os
from datetime import datetime, timedelta, timezone

from fastapi import FastAPI, Header, HTTPException
from fastapi.responses import JSONResponse
from contextlib import asynccontextmanager
from pydantic import BaseModel, Field

try:
    from pymongo import MongoClient
except ImportError:
    MongoClient = None

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("health-api")

# User timezone: IST (UTC+5:30, no DST) as a fixed offset — no tzdata needed.
IST = timezone(timedelta(hours=5, minutes=30))


# Lifespan: close the shared Mongo client on shutdown so the container exits cleanly.
@asynccontextmanager
async def _lifespan(app: FastAPI):
    yield
    global _client
    if _client is not None:
        _client.close()
        _client = None


# App + startup token snapshot (re-read per request via _tokens so rotation needs no restart).
app = FastAPI(title="Health Sync API", lifespan=_lifespan)

_raw_tokens = os.environ.get("PASSWORD", "")
TOKENS = {t.strip() for t in _raw_tokens.split(",") if t.strip()}


def _tokens() -> set[str]:
    """Read accepted tokens fresh (env may rotate without a restart)."""
    raw = os.environ.get("PASSWORD", "")
    return {t.strip() for t in raw.split(",") if t.strip()}

# Shared client (lazily created) + accessor honoring MONGODB_URI/MONGODB_DB from the root .env.
_client: MongoClient | None = None


def _get_db():
    global _client
    if MongoClient is None:
        raise RuntimeError("pymongo not installed")
    uri = os.environ.get("MONGODB_URI", "").strip()
    if not uri:
        raise RuntimeError("MONGODB_URI not set")
    db_name = os.environ.get("MONGODB_DB", "hermes").strip() or "hermes"
    if _client is None:
        _client = MongoClient(uri, serverSelectionTimeoutMS=8000)
    return _client[db_name]


# ── Models matching the Android app's HealthSyncPayload ──
# One sleep session; booked to the wake (IST) date so nights count toward the morning.
class SleepEntry(BaseModel):
    startIso: str
    endIso: str
    totalMinutes: int
    stages: dict[str, int] = Field(default_factory=dict)


# One workout session; deduped downstream on (type, minutes, kcal) per start date.
class WorkoutEntry(BaseModel):
    startIso: str
    endIso: str
    title: str = "Workout"
    type: str = "WORKOUT"
    distanceMeters: float | None = None
    caloriesKcal: float | None = None


# Top-level sync body: today's totals plus bounded sleep/workout batches (max 100 each).
class HealthSyncPayload(BaseModel):
    device: str = "Redmi Watch 5 Lite"
    syncedAtIso: str
    steps: int | None = None
    activeCaloriesKcal: float | None = None
    sleep: list[SleepEntry] = Field(default_factory=list, max_length=100)
    workouts: list[WorkoutEntry] = Field(default_factory=list, max_length=100)


def _local_date(iso: str) -> str:
    """Bucket an ISO timestamp into the user's (IST) calendar date."""
    try:
        dt = datetime.fromisoformat(iso.replace("Z", "+00:00"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.astimezone(IST).date().isoformat()
    except Exception:
        return iso[:10]


# Bearer check against the accepted token set; 503 when the server itself has no tokens configured.
def _authorize(authorization: str | None) -> None:
    tokens = _tokens()
    if not tokens:
        logger.warning("PASSWORD not set — rejecting all requests")
        raise HTTPException(status_code=503, detail="server not configured with tokens")
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="missing bearer token")
    token = authorization[7:].strip()
    if token not in tokens:
        raise HTTPException(status_code=401, detail="invalid token")


# Liveness probe for the container orchestrator (no auth, no DB touch).
@app.get("/health")
async def health():
    return {"status": "ok"}


# Main ingest: upserts today's totals into hc_days and appends deduped sleep/workout sessions.
@app.post("/api/health/sync")
async def sync(payload: HealthSyncPayload, authorization: str | None = Header(None)):
    _authorize(authorization)

    synced_at = datetime.now(timezone.utc).isoformat()
    stats_date = _local_date(payload.syncedAtIso)

    db = _get_db()
    days = db["hc_days"]

    # hc_days: one doc per date — same shape the MCP writes.
    update: dict = {"updatedAt": synced_at}
    if payload.steps is not None:
        update["steps"] = payload.steps
    if payload.activeCaloriesKcal is not None:
        update["active_kcal"] = payload.activeCaloriesKcal
    if update.keys() - {"updatedAt"}:
        days.update_one({"date": stats_date}, {"$set": update}, upsert=True)

    # sleep: accumulate sessions into sleep_hours on the wake date.
    # Idempotent: sessions already recorded (by start timestamp) are skipped,
    # so re-syncs never double-count. NOTE: the MCP's manual log_sleep $sets
    # sleep_hours and therefore overrides the accumulated watch total.
    for s in payload.sleep:
        wake_date = _local_date(s.endIso)
        dup = days.find_one({"date": wake_date, "sleep_sessions.start": s.startIso})
        if dup:
            continue
        hours = round(s.totalMinutes / 60.0, 2)
        days.update_one(
            {"date": wake_date},
            {"$inc": {"sleep_hours": hours},
             "$push": {"sleep_sessions": {"start": s.startIso, "minutes": s.totalMinutes}},
             "$set": {"updatedAt": synced_at}},
            upsert=True,
        )

    # workouts: append new sessions (dedupe on type + minutes + kcal).
    for w in payload.workouts:
        duration = _minutes_between(w.startIso, w.endIso)
        if duration is None:
            logger.warning("skipping workout with unparseable timestamps: %s -> %s",
                           w.startIso, w.endIso)
            continue
        kcal = round(w.caloriesKcal or 0, 1)
        wdoc = {"type": w.type, "minutes": duration, "kcal": kcal}
        dup = days.find_one({
            "date": _local_date(w.startIso),
            "workouts": {"$elemMatch": {"type": w.type, "minutes": duration, "kcal": kcal}},
        })
        if dup:
            continue
        days.update_one({"date": _local_date(w.startIso)},
                        {"$push": {"workouts": wdoc},
                         "$set": {"updatedAt": synced_at}},
                        upsert=True)

    logger.info(
        "synced device=%s steps=%s calories=%s sleep=%d workouts=%d",
        payload.device, payload.steps, payload.activeCaloriesKcal,
        len(payload.sleep), len(payload.workouts),
    )

    return {"status": "ok", "synced_at": synced_at}


# Whole-minute duration between two ISO timestamps; None when unparseable so the caller can skip.
def _minutes_between(start_iso: str, end_iso: str) -> int | None:
    try:
        s = datetime.fromisoformat(start_iso.replace("Z", "+00:00"))
        e = datetime.fromisoformat(end_iso.replace("Z", "+00:00"))
        return max(1, int((e - s).total_seconds() // 60))
    except Exception:
        return None

#!/usr/bin/env bash
set -euo pipefail

# Whole-host Linux usage sampler + reporter.
#
# `record` appends one line per run to run/sysmon/YYYY-MM-DD.tsv with
#   epoch  cpu%  load1  mem%  swap%  disk%
# (CPU% is measured as a 1s delta inside the call, so each cron run is
# self-contained — no state between runs.)
#
# `report [YYYY-MM-DD]` prints current / avg / min / max / p95 for each
# metric from that day's samples (default: today).
#
# `install` / `remove` manage the every-minute sampler crontab entry.

REPO="$(cd "$(dirname "$0")/.." && pwd)"
TSV_DIR="$REPO/run/sysmon"

# shellcheck source=scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"
load_root_env

# Minimal log helpers when run standalone (hermes.sh defines these itself).
command -v warn >/dev/null 2>&1 || warn() { echo -e "\033[1;33m[WARN]\033[0m  $*" >&2; }
command -v info >/dev/null 2>&1 || info() { echo -e "\033[0;32m[INFO]\033[0m  $*" >&2; }

# ── Sample one row ─────────────────────────────────────
sample() {
    local cpu load1 now mem_pct swap_pct disk_pct
    local pre_total pre_idle post_total post_idle dtotal didle

    load1="$(cut -d' ' -f1 /proc/loadavg)"

    # CPU% via two /proc/stat reads ~1s apart (tmp worktables keep /proc/stat
    # deltas local to this call, so each cron run is self-contained).
    read -r pre_total pre_idle < <(awk '/^cpu /{t=0; for(i=2;i<=NF;i++) t+=$i; print t, $5}' /proc/stat)
    sleep 1
    read -r post_total post_idle < <(awk '/^cpu /{t=0; for(i=2;i<=NF;i++) t+=$i; print t, $5}' /proc/stat)

    dtotal=$((post_total - pre_total)); didle=$((post_idle - pre_idle))
    if (( dtotal > 0 )); then
        cpu=$(( (dtotal - didle) * 100 / dtotal ))
    else
        cpu=0
    fi

    read -r total_mem free_mem < <(awk '/MemTotal/{t=$2} /MemAvailable/{a=$2} END{print t, a}' /proc/meminfo)
    total_mem="${total_mem:-0}"; free_mem="${free_mem:-0}"
    if (( total_mem > 0 )); then
        mem_pct=$(( (total_mem - free_mem) * 100 / total_mem ))
    else
        mem_pct=0
    fi

    read -r swap_total swap_free < <(awk '/SwapTotal/{t=$2} /SwapFree/{f=$2} END{print t, f}' /proc/meminfo)
    swap_total="${swap_total:-0}"; swap_free="${swap_free:-0}"
    if (( swap_total > 0 )); then
        swap_pct=$(( (swap_total - swap_free) * 100 / swap_total ))
    else
        swap_pct=0
    fi

    disk_pct="$(df / --output=pcent 2>/dev/null | tail -1 | tr -dc '0-9' || echo 0)"

    now="$(date +%s)"
    # Day file resolved per-call so long-lived shells crossing midnight
    # don't keep appending to yesterday's file. Lock so concurrent
    # manual runs can't interleave short writes.
    {
        flock -n 9 || { warn "sysmon sample already in progress — skipping."; return 0; }
        printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$now" "$cpu" "$load1" "$mem_pct" "$swap_pct" "$disk_pct" >> "$TSV_DIR/$(date +%F).tsv"
    } 9>"$TSV_DIR/.lock"
}

# ── Stats for one day's file ───────────────────────────
stats() {
    local date="${1:-$(date +%F)}" f
    f="$TSV_DIR/$date.tsv"
    [[ -f "$f" ]] || { warn "no samples for $date ($f)"; exit 1; }

    local body
    body="$(python3 - "$f" "$date" <<'PY'
import statistics, sys
f, date = sys.argv[1], sys.argv[2]
rows = []
for line in open(f):
    parts = line.rstrip("\n").split("\t")
    if len(parts) == 6:
        rows.append([float(p) for p in parts])
if not rows:
    print("no rows"); raise SystemExit(0)
cur = rows[-1]
lines = [f"sysmon · {date} · {len(rows)} samples", ""]
labels = ["cpu%", "load1", "mem%", "swap%", "disk%"]
hdr = f"{'':<8}{'cur':>8}{'avg':>8}{'min':>8}{'max':>8}{'p95':>8}"
lines.append(hdr)
lines.append("-" * len(hdr))
for i, label in enumerate(labels, start=1):
    col = [r[i] for r in rows]
    avg = statistics.fmean(col)
    p95 = sorted(col)[int(0.95 * (len(col) - 1))]
    lines.append(f"{label:<8}{cur[i]:>8.1f}{avg:>8.1f}{min(col):>8.1f}{max(col):>8.1f}{p95:>8.1f}")
print("\n".join(lines))
PY
)"
    echo "$body"
}

# ── Cron management ────────────────────────────────────
cron_lines() {
    echo "* * * * * bash $REPO/scripts/sysmon.sh record >> $REPO/run/sysmon.log 2>&1"
}

install_cron() {
    command -v crontab >/dev/null 2>&1 || { warn "crontab not found"; return 1; }
    mkdir -p "$REPO/run"
    {
        flock -n 9 || { warn "cron update already in progress — skipping."; return 0; }
        if crontab -l 2>/dev/null | grep -qF "sysmon.sh record"; then
            info "sysmon cron already installed."
            return 0
        fi
        ( crontab -l 2>/dev/null | grep -vF "sysmon.sh" || true; cron_lines ) | crontab -
        info "sysmon cron installed: sample every minute."
    } 9>"$REPO/run/cron.lock"
}

remove_cron() {
    if command -v crontab >/dev/null 2>&1; then
        mkdir -p "$REPO/run"
        {
            flock -n 9 || { warn "cron update already in progress — skipping."; return 0; }
            ( crontab -l 2>/dev/null | grep -vF "sysmon.sh" || true ) | crontab - || true
            info "sysmon cron removed."
        } 9>"$REPO/run/cron.lock"
    fi
}

# ── Entry ──────────────────────────────────────────────
cmd="${1:-report}"
case "$cmd" in
    record) mkdir -p "$TSV_DIR"; sample ;;
    report) stats "${2:-$(date +%F)}" ;;
    install) install_cron ;;
    remove) remove_cron ;;
    *) warn "usage: $0 {record|report [YYYY-MM-DD]|install|remove}"; exit 1 ;;
esac
package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// maxBodyBytes caps a request body. A chat request is a few kilobytes of messages;
// anything past this is a mistake or an attempt to make the gateway buffer.
const maxBodyBytes = 4 << 20 // 4 MiB

// decodeJSONBody reads and decodes a JSON request body, answering the client
// itself on failure so handlers stay free of error-envelope plumbing.
//
// Unknown fields are allowed: the app sends provider and model, which are
// accepted and ignored, and a stricter parser would reject a newer app.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSON(ct) {
		apiError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "",
			"Content-Type must be application/json")
		return errors.New("gateway: bad content type")
	}

	// http.MaxBytesReader so an oversized body is refused while streaming in,
	// rather than being read into memory first.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		// MaxBytesReader reports its own sentinel when the cap is hit. Distinct
		// from a parse failure, because the client's fix is different: shrink the
		// request rather than fix its syntax.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "",
				"request body exceeds the 4 MiB limit")
			return err
		}
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"could not parse the request body as JSON")
		return err
	}
	// Reject trailing content so a truncated or doubled body is caught here rather
	// than surfacing as a mysteriously incomplete turn.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"unexpected content after the JSON body")
		return errors.New("gateway: trailing content")
	}
	return nil
}

// isJSON reports whether a Content-Type names a JSON media type.
//
// Parsed with mime.ParseMediaType rather than scanned by hand: a hand-rolled
// check accepted anything containing "/" followed by a "j", so a header like
// "garbage with space json" would be treated as JSON.
func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	// A few clients send text/json, and structured-suffix types like
	// application/vnd.api+json are unambiguously JSON too.
	return mediaType == "application/json" ||
		mediaType == "text/json" ||
		strings.HasSuffix(mediaType, "+json")
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

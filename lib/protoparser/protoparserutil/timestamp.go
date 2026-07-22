// Forked from github.com/VictoriaMetrics/VictoriaMetrics v1.146.1-0.20260630165203-c82127b6d4d1
// See lib/httpserver/UPSTREAM.md. Local changes are marked with "VL-FORK:".

package protoparserutil

import (
	"fmt"
	"net/http"
	"strconv"
)

// GetTimestamp extracts unix timestamp in milliseconds from `timestamp` query arg.
//
// It returns 0 if there is no `timestamp` query arg.
func GetTimestamp(req *http.Request) (int64, error) {
	ts := req.URL.Query().Get("timestamp")
	if len(ts) == 0 {
		return 0, nil
	}
	timestamp, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse `timestamp=%s` query arg: %w", ts, err)
	}
	return timestamp, nil
}

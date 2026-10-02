package jsonerr

import (
	"encoding/json"
	"strconv"
	"testing"
)

type unprintableError struct{}

func (unprintableError) Error() string { panic("serialization must not call Err.Error") }

func TestMarshalJSONKeepsUnderlyingErrorsPrivate(t *testing.T) {
	for _, code := range []int{400, 404, 409, 500, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			value := JSONError{Code: code, Message: "public message", Err: unprintableError{}, CorrelationID: "request-id"}
			if code >= 500 {
				value.Message = "private connection details"
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			wantMessage := "public message"
			if code >= 500 {
				wantMessage = "internal error"
			}
			if len(fields) != 3 || fields["message"] != wantMessage || fields["code"] != float64(code) ||
				fields["correlation_id"] != "request-id" {
				t.Fatalf("unexpected public error: %s", encoded)
			}
		})
	}
}

package main

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/pb33f/libopenapi-validator/config"
	"github.com/pb33f/libopenapi-validator/radix"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
)

// contract checks every response the tests receive against openapi.yaml, so a handler that drifts from
// the contract fails the suite. Requests are not checked: tests send bad ones on purpose.
var contract struct {
	sync.Mutex
	validator validator.Validator
	failures  []string
}

func TestMain(m *testing.M) {
	spec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi.yaml:", err)
		os.Exit(1)
	}

	doc, err := libopenapi.NewDocument(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi.yaml:", err)
		os.Exit(1)
	}

	model, err := doc.BuildV3Model()
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi.yaml:", err)
		os.Exit(1)
	}

	v, errs := validator.NewValidator(doc, config.WithPathTree(verbAware{radix.BuildPathTree(&model.Model)}))
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "openapi.yaml:", errs)
		os.Exit(1)
	}

	if ok, errs := v.ValidateDocument(); !ok {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "openapi.yaml:", e.Message, e.Reason)
		}

		os.Exit(1)
	}

	contract.validator = v

	code := m.Run()

	if len(contract.failures) > 0 {
		fmt.Fprintln(os.Stderr, "responses that break openapi.yaml:")

		for _, f := range contract.failures {
			fmt.Fprintln(os.Stderr, " ", f)
		}

		if code == 0 {
			code = 1
		}
	}

	os.Exit(code)
}

// verbAware sends paths ending in an AIP custom verb ("/v1/uploads/{id}:complete") to the validator's
// regex matcher. Its radix tree would take "<id>:complete" for the id of "/v1/uploads/{uploadId}" and
// report the POST as missing.
type verbAware struct{ tree radix.PathLookup }

func (v verbAware) Lookup(urlPath string) (*v3.PathItem, string, bool) {
	if last := urlPath[strings.LastIndex(urlPath, "/")+1:]; strings.Contains(last, ":") {
		return nil, "", false
	}

	return v.tree.Lookup(urlPath)
}

// withContract records the response, forwards it unchanged and validates it against the spec. CORS
// preflights and routes the spec does not know (chi's 404/405) are skipped.
func withContract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)

		res := rec.Result()
		maps.Copy(w.Header(), res.Header)
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(rec.Body.Bytes())

		if r.Method == http.MethodOptions || res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
			return
		}

		res.Body = io.NopCloser(bytes.NewReader(rec.Body.Bytes()))

		if ok, errs := contract.validator.ValidateHttpResponse(r, res); !ok {
			contract.Lock()
			defer contract.Unlock()

			for _, e := range errs {
				contract.failures = append(contract.failures, fmt.Sprintf("%s %s -> %d: %s (%s)", r.Method, r.URL.Path, res.StatusCode, e.Message, e.Reason))
			}
		}
	})
}

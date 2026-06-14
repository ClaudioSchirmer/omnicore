package binding

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// BuildRequest assembles *http.Request from a typed Req value + endpoint
// metadata. The context flows to http.NewRequestWithContext for cancellation
// propagation. baseURL is taken verbatim; the path is concatenated with
// placeholder substitution; query parameters and headers come from the
// tagged fields of req.
//
// Errors at this stage are programmer / configuration mistakes (missing path
// placeholder for a tagged field, unsupported field type, unknown codec) and
// are returned without dialing.
func BuildRequest(ctx context.Context, baseURL string, ep EndpointMeta, req any) (*http.Request, error) {
	if ep.Method == "" {
		return nil, fmt.Errorf("binding: endpoint method required")
	}
	if ep.Path == "" || !strings.HasPrefix(ep.Path, "/") {
		return nil, fmt.Errorf("binding: endpoint path %q must start with '/'", ep.Path)
	}
	rv, err := derefStruct(req)
	if err != nil {
		return nil, err
	}
	plan, err := inspectRequestType(rv.Type(), ep.Path)
	if err != nil {
		return nil, err
	}

	path := ep.Path
	query := url.Values{}
	headers := http.Header{}
	for k, v := range ep.Headers {
		headers.Set(k, v)
	}

	var (
		bodyBytes     []byte
		bodyStream    io.Reader
		contentType   string
		contentLength int64 = -1
	)

	for i, b := range plan.bindings {
		fv := rv.FieldByIndex(b.fieldIndex)
		switch b.kind {
		case bindPath:
			s, err := scalarToString(fv)
			if err != nil {
				return nil, fmt.Errorf("binding: path %q: %w", b.name, err)
			}
			path = strings.ReplaceAll(path, "{"+b.name+"}", url.PathEscape(s))
		case bindQuerySingle:
			s, err := scalarToString(fv)
			if err != nil {
				return nil, fmt.Errorf("binding: query %q: %w", b.name, err)
			}
			if s != "" {
				query.Set(b.name, s)
			}
		case bindQueryCSV:
			parts, err := sliceToStrings(fv)
			if err != nil {
				return nil, fmt.Errorf("binding: query %q (csv): %w", b.name, err)
			}
			if len(parts) > 0 {
				query.Set(b.name, strings.Join(parts, ","))
			}
		case bindQueryMulti:
			parts, err := sliceToStrings(fv)
			if err != nil {
				return nil, fmt.Errorf("binding: query %q (multi): %w", b.name, err)
			}
			for _, v := range parts {
				query.Add(b.name, v)
			}
		case bindHeader:
			s, err := scalarToString(fv)
			if err != nil {
				return nil, fmt.Errorf("binding: header %q: %w", b.name, err)
			}
			if s != "" {
				headers.Set(b.name, s)
			}
		case bindHeadersMap:
			it := fv.MapRange()
			for it.Next() {
				headers.Set(it.Key().String(), it.Value().String())
			}
		case bindBody:
			_ = i // index reserved for future positional logic
			codec, err := codecByName(b.codec)
			if err != nil {
				return nil, err
			}
			data, ct, err := codec.Encode(fv.Interface())
			if err != nil {
				return nil, err
			}
			bodyBytes = data
			contentType = ct
		case bindBodyStream:
			rdr, ok := fv.Interface().(io.Reader)
			if !ok || fv.IsZero() {
				return nil, fmt.Errorf("binding: body,stream field is nil")
			}
			bodyStream = rdr
			// Content-Type is the caller's responsibility for streams —
			// set via http:"header,Content-Type" or WithExtraHeader.
		case bindBodyMultipart:
			r, ct, length, err := encodeMultipartBody(fv.Interface())
			if err != nil {
				return nil, err
			}
			bodyStream = r
			contentType = ct
			contentLength = length
		}
	}

	fullURL := baseURL + path
	if encoded := query.Encode(); encoded != "" {
		if strings.Contains(fullURL, "?") {
			fullURL += "&" + encoded
		} else {
			fullURL += "?" + encoded
		}
	}

	var body io.Reader
	switch {
	case bodyStream != nil:
		body = bodyStream
	case bodyBytes != nil:
		body = bytes.NewReader(bodyBytes)
	}
	httpReq, err := http.NewRequestWithContext(ctx, ep.Method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("binding: build request: %w", err)
	}
	for k, vs := range headers {
		httpReq.Header[k] = vs
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	switch {
	case bodyBytes != nil:
		httpReq.ContentLength = int64(len(bodyBytes))
	case bodyStream != nil && contentLength >= 0:
		httpReq.ContentLength = contentLength
	case bodyStream != nil:
		// Unknown length → chunked transfer. Leave ContentLength at 0
		// so http.NewRequest does the right thing.
		httpReq.ContentLength = 0
	}
	return httpReq, nil
}

// derefStruct returns the struct reflect.Value of req, accepting both
// struct values and *struct. Other kinds error so the caller sees the
// mistake at build time.
func derefStruct(v any) (reflect.Value, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Value{}, fmt.Errorf("binding: request is a nil pointer")
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("binding: request must be a struct, got %s", rv.Kind())
	}
	return rv, nil
}

// scalarToString turns a scalar Go value into a wire-format string. Pointers
// are de-referenced; nil pointers yield "" so optional fields naturally
// vanish from the URL.
func scalarToString(v reflect.Value) (string, error) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64), nil
	}
	return "", fmt.Errorf("unsupported scalar kind %s", v.Kind())
}

func sliceToStrings(v reflect.Value) ([]string, error) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return nil, fmt.Errorf("expected slice or array, got %s", v.Kind())
	}
	out := make([]string, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		s, err := scalarToString(v.Index(i))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

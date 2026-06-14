package binding

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
)

// DecodeResponse populates the value pointed to by out from resp. out must be
// a non-nil pointer to a struct (or to the empty struct{} when the caller
// only cares about the status). The response body is consumed in full and
// closed in every path so the connection can return to the pool.
//
// Field assignment is tag-driven:
//   - http:"header,Name" → resp.Header.Get(Name)
//   - http:"body,<codec>" → decoded via codec
//   - struct without any body tag and at least one header tag → no body
//     decoding (the caller is reading only headers)
//   - struct without any tag (or only headers) → body decoded as the whole
//     struct via ep.ResponseCodec (most common case — declares the response
//     shape directly)
func DecodeResponse(resp *http.Response, ep EndpointMeta, out any) error {
	if resp == nil {
		return fmt.Errorf("binding: nil response")
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("binding: out must be a non-nil pointer to a struct")
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return fmt.Errorf("binding: out must point at a struct, got %s", elem.Kind())
	}
	if elem.NumField() == 0 {
		// struct{} — caller does not want the body.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	plan, err := inspectResponseType(elem.Type())
	if err != nil {
		return err
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("binding: read body: %w", err)
	}

	headerFieldsPresent := false
	for _, b := range plan.bindings {
		if b.kind == bindHeader {
			headerFieldsPresent = true
			fv := elem.FieldByIndex(b.fieldIndex)
			if !fv.CanSet() {
				return fmt.Errorf("binding: header field for %q is not settable", b.name)
			}
			fv.SetString(resp.Header.Get(b.name))
		}
	}

	if plan.hasBody {
		codec, err := codecByName(ep.ResponseCodec)
		if err != nil {
			return err
		}
		bodyField := elem.FieldByIndex(plan.bindings[plan.bodyAt].fieldIndex)
		if !bodyField.CanAddr() {
			return fmt.Errorf("binding: body field is not addressable")
		}
		if err := codec.Decode(bodyBytes, bodyField.Addr().Interface()); err != nil {
			return err
		}
		return nil
	}

	if headerFieldsPresent {
		// Header-only response — caller declared header tags and no body tag.
		// Body bytes are discarded already.
		return nil
	}

	// No tagged fields: decode the whole struct as the body.
	codec, err := codecByName(ep.ResponseCodec)
	if err != nil {
		return err
	}
	return codec.Decode(bodyBytes, out)
}

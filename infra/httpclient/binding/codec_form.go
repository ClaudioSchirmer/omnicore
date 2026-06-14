package binding

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// formCodec encodes/decodes application/x-www-form-urlencoded bodies. The
// codec accepts three input shapes for Encode:
//
//   - url.Values — passed through verbatim
//   - map[string]string and map[string][]string — straight key/value
//   - struct — exported fields become form keys (name lowercased, or
//     overridden via `form:"key[,omitempty]"` tag). nil pointers and zero
//     values are skipped only when `omitempty` is set.
//
// Decode accepts the same shapes (struct via reflection). Fields without
// a tag use the lower-cased field name as the form key.
type formCodec struct{}

const formContentType = "application/x-www-form-urlencoded"

func (formCodec) Encode(v any) ([]byte, string, error) {
	if v == nil {
		return nil, "", nil
	}
	values, err := toValues(v)
	if err != nil {
		return nil, "", err
	}
	return []byte(values.Encode()), formContentType, nil
}

func (formCodec) Decode(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	values, err := url.ParseQuery(string(data))
	if err != nil {
		return fmt.Errorf("decode form-urlencoded: %w", err)
	}
	return fromValues(values, v)
}

// toValues converts the input into url.Values per the rules in the codec
// godoc. Returns an error for unsupported input shapes.
func toValues(v any) (url.Values, error) {
	switch typed := v.(type) {
	case url.Values:
		return typed, nil
	case map[string]string:
		out := url.Values{}
		for k, val := range typed {
			out.Set(k, val)
		}
		return out, nil
	case map[string][]string:
		out := url.Values{}
		for k, vals := range typed {
			out[k] = append([]string(nil), vals...)
		}
		return out, nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return url.Values{}, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("encode form-urlencoded: unsupported input %T", v)
	}
	out := url.Values{}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		key, omitEmpty := formFieldKey(f)
		if key == "-" {
			continue
		}
		fv := rv.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		s, err := scalarToFormString(fv)
		if err != nil {
			return nil, fmt.Errorf("encode form-urlencoded %s.%s: %w", rt.Name(), f.Name, err)
		}
		out.Set(key, s)
	}
	return out, nil
}

// fromValues populates v from url.Values. v may be a pointer to url.Values,
// a pointer to map[string]string, or a pointer to a struct with `form:"key"`
// tags (defaulting to lower-cased field names).
func fromValues(values url.Values, v any) error {
	if v == nil {
		return fmt.Errorf("decode form-urlencoded: nil target")
	}
	switch typed := v.(type) {
	case *url.Values:
		*typed = values
		return nil
	case *map[string]string:
		out := make(map[string]string, len(values))
		for k, vs := range values {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
		*typed = out
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("decode form-urlencoded: out must be a non-nil pointer")
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return fmt.Errorf("decode form-urlencoded: out must point at a struct, url.Values or map")
	}
	rt := elem.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		key, _ := formFieldKey(f)
		if key == "-" {
			continue
		}
		raw := values.Get(key)
		if raw == "" {
			continue
		}
		if err := setFormField(elem.Field(i), raw); err != nil {
			return fmt.Errorf("decode form-urlencoded %s.%s: %w", rt.Name(), f.Name, err)
		}
	}
	return nil
}

// formFieldKey returns the form key for a struct field. Honors `form:"key"`
// (and `form:"key,omitempty"`); defaults to the lower-cased field name.
func formFieldKey(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("form")
	if tag == "" {
		return strings.ToLower(f.Name), false
	}
	parts := strings.Split(tag, ",")
	name := strings.TrimSpace(parts[0])
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	omitEmpty := false
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty
}

// scalarToFormString converts a reflect.Value into the wire string a form
// body expects. Pointers are dereferenced.
func scalarToFormString(v reflect.Value) (string, error) {
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
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.String {
			parts := make([]string, v.Len())
			for i := 0; i < v.Len(); i++ {
				parts[i] = v.Index(i).String()
			}
			return strings.Join(parts, " "), nil
		}
	}
	return "", fmt.Errorf("unsupported field kind %s", v.Kind())
}

// setFormField writes the form value back into a struct field, parsing the
// wire string into the field's Go kind.
func setFormField(v reflect.Value, raw string) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		v.SetFloat(n)
	default:
		return fmt.Errorf("unsupported field kind %s", v.Kind())
	}
	return nil
}

package binding

import (
	"fmt"
	"strings"
)

// bindKind enumerates the destinations a request struct field can target on
// the outbound HTTP request.
type bindKind int

const (
	bindPath        bindKind = iota + 1
	bindQuerySingle          // single query value
	bindQueryCSV             // ?name=a,b,c
	bindQueryMulti           // ?name=a&name=b
	bindHeader               // single header from field value
	bindHeadersMap           // map[string]string field merged into request headers
	bindBody                 // body, codec selected by codec field
	bindBodyStream           // body is an io.Reader piped to the transport as-is
	bindBodyMultipart        // body is httpclient.Multipart (form fields + file streams)
)

// parseHTTPTag interprets the `http:"..."` struct tag and returns the binding
// shape. Returns (zero, false, nil) when the tag is absent so the inspector
// can skip the field. Errors describe what the operator can fix.
//
// Supported shapes:
//
//	http:"path,id"
//	http:"query,verbose"
//	http:"query,tags,csv"
//	http:"query,tags,multi"
//	http:"header,X-Tenant"
//	http:"headers"
//	http:"body,json"   (json is the only codec accepted today)
func parseHTTPTag(tag string) (b fieldBinding, present bool, err error) {
	if tag == "" {
		return fieldBinding{}, false, nil
	}
	parts := strings.Split(tag, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if parts[0] == "" {
		return fieldBinding{}, true, fmt.Errorf("empty http tag")
	}
	switch parts[0] {
	case "path":
		if len(parts) != 2 || parts[1] == "" {
			return fieldBinding{}, true, fmt.Errorf("http:%q: path requires a name (got %q)", tag, tag)
		}
		return fieldBinding{kind: bindPath, name: parts[1]}, true, nil
	case "query":
		if len(parts) < 2 || parts[1] == "" {
			return fieldBinding{}, true, fmt.Errorf("http:%q: query requires a name", tag)
		}
		name := parts[1]
		switch {
		case len(parts) == 2:
			return fieldBinding{kind: bindQuerySingle, name: name}, true, nil
		case len(parts) == 3 && parts[2] == "csv":
			return fieldBinding{kind: bindQueryCSV, name: name}, true, nil
		case len(parts) == 3 && parts[2] == "multi":
			return fieldBinding{kind: bindQueryMulti, name: name}, true, nil
		default:
			return fieldBinding{}, true, fmt.Errorf("http:%q: query style must be omitted, csv or multi", tag)
		}
	case "header":
		if len(parts) != 2 || parts[1] == "" {
			return fieldBinding{}, true, fmt.Errorf("http:%q: header requires a name", tag)
		}
		return fieldBinding{kind: bindHeader, name: parts[1]}, true, nil
	case "headers":
		if len(parts) != 1 {
			return fieldBinding{}, true, fmt.Errorf("http:%q: headers takes no argument", tag)
		}
		return fieldBinding{kind: bindHeadersMap}, true, nil
	case "body":
		if len(parts) != 2 || parts[1] == "" {
			return fieldBinding{}, true, fmt.Errorf("http:%q: body requires a codec name (got %q)", tag, tag)
		}
		codec := parts[1]
		switch codec {
		case "json", "xml", "form", "form-urlencoded":
		case "stream":
			return fieldBinding{kind: bindBodyStream}, true, nil
		case "multipart":
			return fieldBinding{kind: bindBodyMultipart}, true, nil
		default:
			return fieldBinding{}, true, fmt.Errorf("http:%q: body codec %q is not one of json|xml|form|form-urlencoded|stream|multipart", tag, codec)
		}
		// "form" is a convenience alias for "form-urlencoded" on the tag
		// side — the YAML schema continues to spell out the full name.
		if codec == "form" {
			codec = "form-urlencoded"
		}
		return fieldBinding{kind: bindBody, codec: codec}, true, nil
	default:
		return fieldBinding{}, true, fmt.Errorf("http:%q: unsupported kind %q", tag, parts[0])
	}
}

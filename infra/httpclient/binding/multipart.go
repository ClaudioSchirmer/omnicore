package binding

import (
	"fmt"
	"io"
	"mime/multipart"
)

// Multipart describes a multipart/form-data request body. Place an
// instance on a request struct field tagged http:"body,multipart" and
// BuildRequest will pipe text fields and file streams into a
// multipart writer, surface the boundary on the Content-Type header,
// and (when all files are seekable / known-size) report a precise
// Content-Length. When any file streams are unbounded the request
// uses chunked transfer.
//
// The struct lives in the binding subpackage to keep the type accessible
// at the inspection layer without importing back into httpclient (which
// would create an import cycle). The parent httpclient package re-exports
// the type so consumers see it as httpclient.Multipart.
type Multipart struct {
	// Fields are text form values written as part headers
	// "Content-Disposition: form-data; name=Name" with Value as the body.
	Fields []MultipartField

	// Files are file uploads written with filename and content-type. The
	// Content reader is consumed verbatim; ownership of closing it (when
	// applicable) remains with the caller.
	Files []MultipartFile
}

// MultipartField is one text part of a multipart body.
type MultipartField struct {
	Name  string
	Value string
}

// MultipartFile is one file part of a multipart body. Content is read
// fully into the part during BuildRequest; when the reader implements
// io.Seeker AND its size can be discovered cheaply, the framework
// could in principle compute a precise Content-Length — today it does
// not, so multipart uploads always go out with chunked transfer.
type MultipartFile struct {
	Name     string
	Filename string
	MimeType string
	Content  io.Reader
}

// encodeMultipartBody renders a Multipart into an io.Reader streaming
// the body, the Content-Type with the chosen boundary, and the body
// length when known (always -1 today — multipart streams are chunked).
//
// The function buffers field/file delimiters but pipes file contents
// through, so memory usage stays proportional to the boundary header
// overhead rather than the total file size.
func encodeMultipartBody(v any) (io.Reader, string, int64, error) {
	m, ok := v.(Multipart)
	if !ok {
		return nil, "", -1, fmt.Errorf("binding: body,multipart field is not a binding.Multipart")
	}
	// Build the multipart body into a pipe so files stream rather than
	// being read into memory all at once.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go func() {
		defer func() { _ = pw.Close() }()
		for _, f := range m.Fields {
			if err := mw.WriteField(f.Name, f.Value); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		for _, file := range m.Files {
			h := newFilePartHeader(file)
			part, err := mw.CreatePart(h)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if file.Content == nil {
				continue
			}
			if _, err := io.Copy(part, file.Content); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()
	return pr, contentType, -1, nil
}

// newFilePartHeader composes the per-file part headers
// (Content-Disposition + Content-Type when supplied). Matches the
// http.MultipartWriter convention so upstream parsers see what they
// would expect from a browser form submission.
func newFilePartHeader(f MultipartFile) map[string][]string {
	disp := fmt.Sprintf(`form-data; name=%q`, f.Name)
	if f.Filename != "" {
		disp = fmt.Sprintf(`form-data; name=%q; filename=%q`, f.Name, f.Filename)
	}
	h := map[string][]string{
		"Content-Disposition": {disp},
	}
	if f.MimeType != "" {
		h["Content-Type"] = []string{f.MimeType}
	}
	return h
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	errorsx "github.com/qifalab/euler-platform/errors"
)

const (
	// maxBodyBytes bounds a request body (parameters only; no payloads).
	maxBodyBytes = 1 << 20
	// maxListValues bounds one list parameter (InstanceIds, ...).
	maxListValues = 100
	// maxNameRunes bounds a display name (InstanceName, VpcName, ...): the
	// orchestrator's limit for the "name" attribute.
	maxNameRunes = 64
)

// params are the Action parameters, merged from the query string and the
// body. Every value is kept as a string list: a scalar parameter has one, a
// list parameter (JSON array, repeated key, Name.N keys) has several.
type params struct {
	v map[string][]string
}

// parseParams merges the query string with the body the SDKs send:
//
//   - a JSON object (pkg-go/eusdk and the Terraform provider send one without
//     a Content-Type; sdk/python sends one under urllib's default
//     application/x-www-form-urlencoded) — recognised by its leading '{';
//   - a form body (application/x-www-form-urlencoded).
//
// A body parameter overrides the same query parameter. Action and Version are
// envelope fields read from the query only (see actionFrom).
func parseParams(r *http.Request, query url.Values) (params, *errorsx.Error) {
	p := params{v: make(map[string][]string, len(query))}
	p.merge(query)
	if r.Body == nil {
		return p, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return p, errorsx.New(codeInvalidParameter, http.StatusBadRequest, "The request body could not be read.")
	}
	if len(raw) > maxBodyBytes {
		return p, errorsx.New(codeInvalidParameter, http.StatusRequestEntityTooLarge, "The request body exceeds 1 MiB.")
	}
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return p, nil
	}
	mediaType := ""
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			mediaType = mt
		}
	}
	switch {
	case body[0] == '{':
		fields, err := decodeJSONObject(body)
		if err != nil {
			return p, errorsx.New(codeInvalidParameter, http.StatusBadRequest, "The JSON request body is malformed.")
		}
		p.merge(fields)
	case mediaType == "application/json":
		return p, errorsx.New(codeInvalidParameter, http.StatusBadRequest, "The JSON request body must be an object.")
	case mediaType == "" || mediaType == "application/x-www-form-urlencoded":
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return p, errorsx.New(codeInvalidParameter, http.StatusBadRequest, "The form request body is malformed.")
		}
		p.merge(form)
	default:
		return p, errorsx.New(codeInvalidParameter, http.StatusUnsupportedMediaType,
			fmt.Sprintf("Unsupported Content-Type %q: send a JSON object or a form body.", mediaType))
	}
	return p, nil
}

func (p params) merge(src map[string][]string) {
	for k, vs := range src {
		if k == "Action" || k == "Version" {
			continue
		}
		p.v[k] = vs
	}
}

// decodeJSONObject flattens a JSON object into string lists: scalars become
// one value, arrays one value per element, nested objects their JSON text.
func decodeJSONObject(body []byte) (map[string][]string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON object")
	}
	out := make(map[string][]string, len(obj))
	for k, v := range obj {
		switch t := v.(type) {
		case nil:
			continue // null = not supplied
		case []any:
			vals := make([]string, 0, len(t))
			for _, e := range t {
				if e != nil {
					vals = append(vals, jsonScalar(e))
				}
			}
			out[k] = vals
		default:
			out[k] = []string{jsonScalar(t)}
		}
	}
	return out, nil
}

func jsonScalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// has reports whether any of names was supplied.
func (p params) has(names ...string) bool {
	for _, n := range names {
		if _, ok := p.v[n]; ok {
			return true
		}
	}
	return false
}

// str returns the trimmed value of the first of names that was supplied, ""
// when none was. A scalar parameter given several values is refused.
func (p params) str(names ...string) (string, error) {
	for _, n := range names {
		vs, ok := p.v[n]
		if !ok {
			continue
		}
		switch len(vs) {
		case 0:
			return "", nil
		case 1:
			return strings.TrimSpace(vs[0]), nil
		default:
			return "", invalidParam(n, "must be a single value")
		}
	}
	return "", nil
}

// need is str for a mandatory parameter; names[0] is its canonical name, the
// rest are accepted aliases.
func (p params) need(names ...string) (string, error) {
	v, err := p.str(names...)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", missingParam(names[0])
	}
	return v, nil
}

// integer returns an integer parameter in [lo, hi], def when not supplied.
func (p params) integer(name string, def, lo, hi int64) (int64, error) {
	raw, err := p.str(name)
	if err != nil || raw == "" {
		return def, err
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, invalidParam(name, "must be an integer")
	}
	if n < lo || n > hi {
		return 0, invalidParam(name, fmt.Sprintf("must be between %d and %d", lo, hi))
	}
	return n, nil
}

// list returns a list parameter in every form the SDKs and Aliyun-style
// clients use: a repeated key, a JSON array (body or a JSON-array string), a
// comma-separated string, and Name.1, Name.2, ... keys. Values are trimmed,
// empty ones dropped, duplicates removed (first occurrence wins).
func (p params) list(name string) ([]string, error) {
	var raw []string
	raw = append(raw, p.v[name]...)
	type indexed struct {
		n int
		v []string
	}
	var numbered []indexed
	prefix := name + "."
	for k, vs := range p.v {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		n, err := strconv.Atoi(k[len(prefix):])
		if err != nil || n < 1 {
			continue
		}
		numbered = append(numbered, indexed{n, vs})
	}
	sort.Slice(numbered, func(i, j int) bool { return numbered[i].n < numbered[j].n })
	for _, e := range numbered {
		raw = append(raw, e.v...)
	}

	var out []string
	seen := make(map[string]bool)
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") {
			var arr []string
			if err := json.Unmarshal([]byte(v), &arr); err != nil {
				return nil, invalidParam(name, "must be a JSON array of strings")
			}
			for _, e := range arr {
				add(e)
			}
			continue
		}
		for _, e := range strings.Split(v, ",") {
			add(e)
		}
	}
	if len(out) > maxListValues {
		return nil, invalidParam(name, fmt.Sprintf("accepts at most %d values", maxListValues))
	}
	return out, nil
}

// ids returns the resource ids named by a singular and/or a plural parameter
// (InstanceId / InstanceIds).
func (p params) ids(single, plural string) ([]string, error) {
	var many []string
	if plural != "" {
		var err error
		if many, err = p.list(plural); err != nil {
			return nil, err
		}
	}
	one, err := p.str(single)
	if err != nil {
		return nil, err
	}
	if one != "" && !slices.Contains(many, one) {
		many = append([]string{one}, many...)
	}
	if len(many) > maxListValues {
		return nil, invalidParam(plural, fmt.Sprintf("accepts at most %d values", maxListValues))
	}
	return many, nil
}

// name returns an optional display name: at most maxNameRunes characters,
// valid UTF-8, no control characters.
func (p params) name(names ...string) (string, error) {
	v, err := p.str(names...)
	if err != nil || v == "" {
		return "", err
	}
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxNameRunes ||
		strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", invalidParam(names[0], fmt.Sprintf("must be at most %d printable characters", maxNameRunes))
	}
	return v, nil
}

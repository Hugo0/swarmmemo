package references

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxSafeInteger = int64(9007199254740991)

var integerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// Canonical encodes the reference profile's scalar-valid, integer-only JSON.
// It never normalizes text or accepts an already rounded floating-point number.
func Canonical(value any) ([]byte, error) {
	if err := validValue(reflect.ValueOf(value), 0); err != nil {
		return nil, ErrUnavailable
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnavailable
	}
	parsed, err := parseJSON(raw)
	if err != nil {
		return nil, ErrUnavailable
	}
	return encodeParsedJSON(parsed)
}

// encodeParsedJSON accepts only an unmodified tree from a successful parseJSON.
// That parser has already checked duplicates, scalars, integers, depth and nodes;
// arbitrary Go values must continue to enter through Canonical instead.
func encodeParsedJSON(parsed any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(parsed) != nil {
		return nil, ErrUnavailable
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func validValue(value reflect.Value, depth int) error {
	if depth > 32 {
		return ErrUnavailable
	}
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return validValue(value.Elem(), depth)
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return ErrUnavailable
		}
	case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return ErrUnavailable
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return ErrUnavailable
		}
		iter := value.MapRange()
		for iter.Next() {
			if validValue(iter.Key(), depth+1) != nil || validValue(iter.Value(), depth+1) != nil {
				return ErrUnavailable
			}
		}
	case reflect.Array, reflect.Slice:
		for index := 0; index < value.Len(); index++ {
			if validValue(value.Index(index), depth+1) != nil {
				return ErrUnavailable
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath == "" && field.Tag.Get("json") != "-" && validValue(value.Field(index), depth+1) != nil {
				return ErrUnavailable
			}
		}
	}
	return nil
}

// encoding/json replaces malformed UTF-16 escapes. Reject those before decoding
// so Python and Go cannot silently assign different identities to malformed data.
func validStringScalars(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	quoted := false
	for index := 0; index < len(raw); index++ {
		if raw[index] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || raw[index] != '\\' {
			continue
		}
		index++
		if index >= len(raw) {
			return false
		}
		if raw[index] != 'u' {
			continue
		}
		if index+4 >= len(raw) {
			return false
		}
		code, err := strconv.ParseUint(string(raw[index+1:index+5]), 16, 16)
		if err != nil {
			return false
		}
		index += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[index+3:index+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			index += 6
		}
	}
	return !quoted
}

func parseJSON(raw []byte) (any, error) {
	if !validStringScalars(raw) {
		return nil, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		nodes++
		if depth > 32 || nodes > 150000 {
			return nil, ErrUnavailable
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrUnavailable
		}
		switch token := token.(type) {
		case json.Delim:
			if token == '{' {
				object := map[string]any{}
				for decoder.More() {
					key, err := decoder.Token()
					if err != nil {
						return nil, ErrUnavailable
					}
					name, ok := key.(string)
					if !ok {
						return nil, ErrUnavailable
					}
					if _, exists := object[name]; exists {
						return nil, ErrUnavailable
					}
					value, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					object[name] = value
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim('}') {
					return nil, ErrUnavailable
				}
				return object, nil
			}
			if token == '[' {
				array := []any{}
				for decoder.More() {
					value, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					array = append(array, value)
				}
				end, err := decoder.Token()
				if err != nil || end != json.Delim(']') {
					return nil, ErrUnavailable
				}
				return array, nil
			}
			return nil, ErrUnavailable
		case json.Number:
			if !integerPattern.MatchString(string(token)) {
				return nil, ErrUnavailable
			}
			value, err := token.Int64()
			if err != nil || value > maxSafeInteger {
				return nil, ErrUnavailable
			}
			return token, nil
		case string, bool, nil:
			return token, nil
		}
		return nil, ErrUnavailable
	}
	value, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, ErrUnavailable
	}
	return value, nil
}

// ReferenceID frames source and external IDs; it is not a source signature.
func ReferenceID(sourceID, externalID string) (string, error) {
	if !sourcePattern.MatchString(sourceID) || !boundedText(externalID, 512) || externalID == "" {
		return "", ErrUnavailable
	}
	raw, err := Canonical([]string{"swarmmemo.reference.v1", sourceID, externalID})
	if err != nil {
		return "", err
	}
	return hash(raw), nil
}

func boundedText(value string, maximum int) bool {
	return utf8.ValidString(value) && len(value) <= maximum && !strings.ContainsRune(value, 0)
}

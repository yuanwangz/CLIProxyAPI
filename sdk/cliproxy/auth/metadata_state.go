package auth

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	metadataCredentialCreatedAtKey = "credential_created_at"
	metadataDisabledKey            = "disabled"
	metadataLastErrorKey           = "last_error"
	metadataStatusMessageKey       = "status_message"
)

// PrepareAuthMetadataForSave fills durable auth metadata before a store writes
// the credential payload.
func PrepareAuthMetadataForSave(auth *Auth, path string) {
	if auth == nil {
		return
	}
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = ResolveCredentialCreatedAtFromFile(path, time.Now().UTC())
	}
	SyncAuthStateToMetadata(auth)
}

// SyncAuthStateToMetadata stores lightweight runtime status fields beside the
// provider tokens so management pages can preserve failure classification after reloads.
func SyncAuthStateToMetadata(auth *Auth) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if auth.CreatedAt.IsZero() {
		if createdAt, ok := CredentialCreatedAtFromMetadata(auth.Metadata); ok {
			auth.CreatedAt = createdAt
		} else {
			auth.CreatedAt = time.Now().UTC()
		}
	}
	if !auth.CreatedAt.IsZero() {
		auth.Metadata[metadataCredentialCreatedAtKey] = auth.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	auth.Metadata[metadataDisabledKey] = auth.Disabled
	persistLastError := auth.Disabled || auth.Status == StatusDisabled
	if persistLastError && auth.LastError != nil && auth.LastError.HTTPStatus == 401 {
		auth.StatusMessage = firstNonEmptyAuthStateString(auth.StatusMessage, "unauthorized")
	}
	if persistLastError && auth.LastError != nil {
		lastErrorMetadata := authErrorToMetadata(auth.LastError)
		if len(lastErrorMetadata) > 0 {
			auth.Metadata[metadataLastErrorKey] = lastErrorMetadata
		} else {
			delete(auth.Metadata, metadataLastErrorKey)
		}
	} else {
		delete(auth.Metadata, metadataLastErrorKey)
	}

	statusMessage := strings.TrimSpace(auth.StatusMessage)
	if statusMessage != "" && (auth.Disabled || auth.Status == StatusDisabled || auth.LastError != nil) {
		auth.Metadata[metadataStatusMessageKey] = statusMessage
		return
	}
	delete(auth.Metadata, metadataStatusMessageKey)
}

// RestoreAuthStateFromMetadata restores persisted runtime status fields from auth metadata.
func RestoreAuthStateFromMetadata(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	if createdAt, ok := CredentialCreatedAtFromMetadata(auth.Metadata); ok {
		auth.CreatedAt = createdAt
	}
	if disabled, ok := metadataBool(auth.Metadata[metadataDisabledKey]); ok {
		auth.Disabled = disabled
		if disabled {
			auth.Status = StatusDisabled
		}
	}
	if !auth.Disabled && auth.Status != StatusDisabled {
		return
	}
	if statusMessage := metadataString(auth.Metadata[metadataStatusMessageKey]); statusMessage != "" && strings.TrimSpace(auth.StatusMessage) == "" {
		auth.StatusMessage = statusMessage
	}
	lastError := authErrorFromMetadata(auth.Metadata[metadataLastErrorKey])
	if lastError == nil {
		return
	}
	if lastError.HTTPStatus == 401 && strings.TrimSpace(lastError.Code) == "" {
		lastError.Code = "unauthorized"
	}
	if lastError.HTTPStatus == 401 && strings.TrimSpace(lastError.Message) == "" {
		lastError.Message = "unauthorized"
	}
	auth.LastError = lastError
	if strings.TrimSpace(auth.StatusMessage) == "" {
		auth.StatusMessage = firstNonEmptyAuthStateString(lastError.Code, lastError.Message)
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		auth.Status = StatusDisabled
		if lastError.HTTPStatus == 401 && strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "unauthorized"
		}
	}
}

// CredentialCreatedAtFromMetadata reads the durable credential creation time
// stored in auth JSON metadata.
func CredentialCreatedAtFromMetadata(metadata map[string]any) (time.Time, bool) {
	if len(metadata) == 0 {
		return time.Time{}, false
	}
	for _, key := range []string{metadataCredentialCreatedAtKey, "credentialCreatedAt"} {
		if createdAt, ok := metadataTime(metadata[key]); ok {
			return createdAt, true
		}
	}
	return time.Time{}, false
}

// CredentialCreatedAt resolves the best available creation time for an auth file.
func CredentialCreatedAt(metadata map[string]any, info os.FileInfo, fallback time.Time) time.Time {
	if createdAt, ok := CredentialCreatedAtFromMetadata(metadata); ok {
		return createdAt
	}
	if createdAt := FileInfoCreatedAt(info); !createdAt.IsZero() {
		return createdAt
	}
	return fallback
}

// ResolveCredentialCreatedAtFromFile reads an existing auth JSON file and returns
// its persisted credential creation time, falling back to filesystem creation time.
func ResolveCredentialCreatedAtFromFile(path string, fallback time.Time) time.Time {
	path = strings.TrimSpace(path)
	if path == "" {
		return fallback
	}
	var metadata map[string]any
	if raw, errRead := os.ReadFile(path); errRead == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &metadata)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		return fallback
	}
	return CredentialCreatedAt(metadata, info, fallback)
}

// FileInfoCreatedAt returns the filesystem birth time when the platform exposes
// one, falling back to ModTime for legacy files without metadata.
func FileInfoCreatedAt(info os.FileInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	if createdAt := birthTimeFromSys(info.Sys()); !createdAt.IsZero() {
		return createdAt.UTC()
	}
	return info.ModTime().UTC()
}

func birthTimeFromSys(sys any) time.Time {
	if sys == nil {
		return time.Time{}
	}
	value := reflect.ValueOf(sys)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return time.Time{}
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return time.Time{}
	}
	for _, fieldName := range []string{"Birthtimespec", "Birthtim", "BirthTime", "CreationTime"} {
		field := value.FieldByName(fieldName)
		if !field.IsValid() {
			continue
		}
		if ts := timeFromStatField(field); !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}

func timeFromStatField(field reflect.Value) time.Time {
	if field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return time.Time{}
		}
		field = field.Elem()
	}
	if field.Kind() == reflect.Struct {
		sec := int64Field(field, "Sec")
		nsec := int64Field(field, "Nsec")
		if sec > 0 {
			return time.Unix(sec, nsec).UTC()
		}
		if low := uint64Field(field, "LowDateTime"); low > 0 {
			high := uint64Field(field, "HighDateTime")
			const windowsToUnix100NS = 116444736000000000
			ticks := (high << 32) | low
			if ticks > windowsToUnix100NS {
				nanos := int64((ticks - windowsToUnix100NS) * 100)
				return time.Unix(0, nanos).UTC()
			}
		}
	}
	if isIntegerKind(field.Kind()) {
		raw := reflectValueInt64(field)
		if raw > 0 {
			return numericTime(raw)
		}
	}
	return time.Time{}
}

func int64Field(value reflect.Value, name string) int64 {
	field := value.FieldByName(name)
	if !field.IsValid() || !isIntegerKind(field.Kind()) {
		return 0
	}
	return reflectValueInt64(field)
}

func uint64Field(value reflect.Value, name string) uint64 {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		raw := field.Int()
		if raw > 0 {
			return uint64(raw)
		}
	}
	return 0
}

func isIntegerKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	default:
		return false
	}
}

func reflectValueInt64(value reflect.Value) int64 {
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		raw := value.Uint()
		if raw <= uint64(^uint64(0)>>1) {
			return int64(raw)
		}
	}
	return 0
}

func authErrorToMetadata(err *Error) map[string]any {
	if err == nil {
		return nil
	}
	result := make(map[string]any, 4)
	if code := strings.TrimSpace(err.Code); code != "" {
		result["code"] = code
	}
	if message := strings.TrimSpace(err.Message); message != "" {
		result["message"] = message
	}
	if err.Retryable {
		result["retryable"] = true
	}
	if err.HTTPStatus > 0 {
		result["http_status"] = err.HTTPStatus
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func authErrorFromMetadata(value any) *Error {
	switch v := value.(type) {
	case nil:
		return nil
	case *Error:
		return cloneError(v)
	case Error:
		return cloneError(&v)
	case map[string]any:
		return authErrorFromMap(v)
	case map[string]string:
		converted := make(map[string]any, len(v))
		for key, item := range v {
			converted[key] = item
		}
		return authErrorFromMap(converted)
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			return authErrorFromMap(parsed)
		}
		return &Error{Message: trimmed}
	default:
		return nil
	}
}

func authErrorFromMap(values map[string]any) *Error {
	if len(values) == 0 {
		return nil
	}
	err := &Error{
		Code:       firstNonEmptyAuthStateString(metadataString(values["code"]), metadataString(values["error_code"])),
		Message:    firstNonEmptyAuthStateString(metadataString(values["message"]), metadataString(values["error"])),
		Retryable:  metadataBoolDefault(values["retryable"]),
		HTTPStatus: firstPositiveInt(values["http_status"], values["status_code"], values["httpStatus"], values["statusCode"]),
	}
	if err.Code == "" && err.Message == "" && err.HTTPStatus == 0 && !err.Retryable {
		return nil
	}
	return err
}

func metadataString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strings.TrimSpace(strconv.FormatFloat(v, 'f', -1, 64))
	default:
		return ""
	}
}

func metadataTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		if !v.IsZero() {
			return v.UTC(), true
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"}
		for _, layout := range layouts {
			if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
				return parsed.UTC(), true
			}
		}
		if raw, errParse := strconv.ParseInt(trimmed, 10, 64); errParse == nil && raw > 0 {
			return numericTime(raw), true
		}
	case json.Number:
		if raw, errParse := v.Int64(); errParse == nil && raw > 0 {
			return numericTime(raw), true
		}
	case int:
		if v > 0 {
			return numericTime(int64(v)), true
		}
	case int64:
		if v > 0 {
			return numericTime(v), true
		}
	case float64:
		if v > 0 {
			return numericTime(int64(v)), true
		}
	}
	return time.Time{}, false
}

func numericTime(raw int64) time.Time {
	switch {
	case raw > 1e17:
		return time.Unix(0, raw).UTC()
	case raw > 1e14:
		return time.Unix(0, raw*int64(time.Microsecond)).UTC()
	case raw > 1e11:
		return time.UnixMilli(raw).UTC()
	default:
		return time.Unix(raw, 0).UTC()
	}
}

func metadataBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		if err == nil {
			return parsed, true
		}
	case int:
		return v != 0, true
	case int64:
		return v != 0, true
	case float64:
		return v != 0, true
	case json.Number:
		parsed, err := strconv.ParseInt(v.String(), 10, 64)
		if err == nil {
			return parsed != 0, true
		}
	}
	return false, false
}

func metadataBoolDefault(value any) bool {
	parsed, ok := metadataBool(value)
	return ok && parsed
}

func firstPositiveInt(values ...any) int {
	for _, value := range values {
		switch v := value.(type) {
		case int:
			if v > 0 {
				return v
			}
		case int64:
			if v > 0 {
				return int(v)
			}
		case float64:
			if v > 0 {
				return int(v)
			}
		case json.Number:
			parsed, err := strconv.ParseInt(v.String(), 10, 64)
			if err == nil && parsed > 0 {
				return int(parsed)
			}
		case string:
			parsed, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func firstNonEmptyAuthStateString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

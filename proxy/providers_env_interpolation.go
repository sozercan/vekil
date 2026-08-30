package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
)

type providerEnvInterpolationMode int

const (
	providerEnvInterpolationLocal providerEnvInterpolationMode = iota
	providerEnvInterpolationRemote
)

type providerEnvWalkContext struct {
	path         string
	providerID   string
	providerPath string
}

var (
	providerConfigReflectType = reflect.TypeOf(ProviderConfig{})
	jsonRawMessageReflectType = reflect.TypeOf(json.RawMessage{})
)

func interpolateProvidersConfigEnv(cfg *ProvidersConfig, remote bool) error {
	mode := providerEnvInterpolationLocal
	if remote {
		mode = providerEnvInterpolationRemote
	}
	return interpolateProviderConfigStrings(cfg, mode)
}

func interpolateProviderConfigStrings(target any, mode providerEnvInterpolationMode) error {
	root := reflect.ValueOf(target)
	if !root.IsValid() || root.Kind() != reflect.Pointer || root.IsNil() {
		return fmt.Errorf("provider config interpolation target must be a non-nil pointer")
	}
	resolved, err := interpolateProviderConfigValue(root.Elem(), providerEnvWalkContext{}, mode)
	if err != nil {
		return err
	}
	root.Elem().Set(resolved)
	return nil
}

func interpolateProviderConfigValue(value reflect.Value, context providerEnvWalkContext, mode providerEnvInterpolationMode) (reflect.Value, error) {
	if !value.IsValid() {
		return value, nil
	}

	switch value.Kind() {
	case reflect.String:
		resolved, err := interpolateProviderConfigString(value.String(), context, mode)
		if err != nil {
			return reflect.Value{}, err
		}
		copy := reflect.New(value.Type()).Elem()
		copy.SetString(resolved)
		return copy, nil
	case reflect.Pointer:
		if value.IsNil() {
			return value, nil
		}
		resolved, err := interpolateProviderConfigValue(value.Elem(), context, mode)
		if err != nil {
			return reflect.Value{}, err
		}
		copy := reflect.New(value.Type().Elem())
		copy.Elem().Set(resolved)
		return copy, nil
	case reflect.Interface:
		if value.IsNil() {
			return value, nil
		}
		resolved, err := interpolateProviderConfigValue(value.Elem(), context, mode)
		if err != nil {
			return reflect.Value{}, err
		}
		copy := reflect.New(value.Type()).Elem()
		copy.Set(resolved)
		return copy, nil
	case reflect.Struct:
		copy := reflect.New(value.Type()).Elem()
		copy.Set(value)
		if value.Type() == providerConfigReflectType {
			providerID := strings.TrimSpace(value.FieldByName("ID").String())
			if providerID != "" && !strings.Contains(providerID, "${env:") {
				context.providerID = providerID
				context.providerPath = context.path
			}
		}
		for index := 0; index < value.NumField(); index++ {
			fieldType := value.Type().Field(index)
			if fieldType.PkgPath != "" {
				continue
			}
			fieldName, skip := providerConfigFieldName(fieldType)
			if skip || fieldName == "api_key_env" {
				continue
			}
			fieldContext := context
			fieldContext.path = appendProviderConfigPath(context.path, fieldName)
			resolved, err := interpolateProviderConfigValue(value.Field(index), fieldContext, mode)
			if err != nil {
				return reflect.Value{}, err
			}
			copy.Field(index).Set(resolved)
		}
		return copy, nil
	case reflect.Slice:
		if value.IsNil() || value.Type() == jsonRawMessageReflectType || value.Type().Elem().Kind() == reflect.Uint8 {
			return value, nil
		}
		copy := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			itemContext := context
			itemContext.path = fmt.Sprintf("%s[%d]", context.path, index)
			resolved, err := interpolateProviderConfigValue(value.Index(index), itemContext, mode)
			if err != nil {
				return reflect.Value{}, err
			}
			copy.Index(index).Set(resolved)
		}
		return copy, nil
	case reflect.Array:
		copy := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			itemContext := context
			itemContext.path = fmt.Sprintf("%s[%d]", context.path, index)
			resolved, err := interpolateProviderConfigValue(value.Index(index), itemContext, mode)
			if err != nil {
				return reflect.Value{}, err
			}
			copy.Index(index).Set(resolved)
		}
		return copy, nil
	case reflect.Map:
		if value.IsNil() {
			return value, nil
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key()
			itemContext := context
			if key.Kind() == reflect.String {
				itemContext.path = fmt.Sprintf("%s[%q]", context.path, key.String())
			}
			resolved, err := interpolateProviderConfigValue(iterator.Value(), itemContext, mode)
			if err != nil {
				return reflect.Value{}, err
			}
			copy.SetMapIndex(key, resolved)
		}
		return copy, nil
	default:
		return value, nil
	}
}

func interpolateProviderConfigString(input string, context providerEnvWalkContext, mode providerEnvInterpolationMode) (string, error) {
	var output strings.Builder
	for offset := 0; offset < len(input); {
		escaped := input[offset] == '\\' && strings.HasPrefix(input[offset+1:], "${env:")
		if !escaped && !strings.HasPrefix(input[offset:], "${env:") {
			output.WriteByte(input[offset])
			offset++
			continue
		}

		start := offset
		if escaped {
			start++
		}
		endRelative := strings.IndexByte(input[start:], '}')
		if endRelative < 0 {
			return "", providerEnvInterpolationSyntaxError(context, "is missing a closing brace")
		}
		end := start + endRelative
		name := input[start+len("${env:") : end]
		if !validProviderEnvName(name) {
			return "", providerEnvInterpolationSyntaxError(context, "must use ${env:VAR_NAME} with a shell-compatible variable name; default values are not supported")
		}
		if escaped {
			output.WriteString(input[start : end+1])
			offset = end + 1
			continue
		}
		if mode == providerEnvInterpolationRemote {
			return "", fmt.Errorf("%s uses env interpolation, which is not allowed in HTTP(S)-loaded provider configs", providerEnvFieldDescription(context))
		}
		value, found := os.LookupEnv(name)
		if !found {
			return "", fmt.Errorf("%s references undefined env var %s", providerEnvFieldDescription(context), name)
		}
		if value == "" {
			return "", fmt.Errorf("%s references empty env var %s", providerEnvFieldDescription(context), name)
		}
		output.WriteString(value)
		offset = end + 1
	}
	return output.String(), nil
}

func validProviderEnvName(name string) bool {
	if name == "" || !(name[0] == '_' || name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		if character != '_' && !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func providerEnvInterpolationSyntaxError(context providerEnvWalkContext, message string) error {
	return fmt.Errorf("%s contains malformed env interpolation: %s", providerEnvFieldDescription(context), message)
}

func providerEnvFieldDescription(context providerEnvWalkContext) string {
	path := context.path
	if context.providerID != "" && context.providerPath != "" {
		path = strings.TrimPrefix(path, context.providerPath+".")
		return fmt.Sprintf("provider %q field %q", context.providerID, path)
	}
	return fmt.Sprintf("field %q", path)
}

func providerConfigFieldName(field reflect.StructField) (string, bool) {
	for _, tagName := range []string{"json", "yaml"} {
		tag := strings.Split(field.Tag.Get(tagName), ",")[0]
		if tag == "-" {
			return "", true
		}
		if tag != "" {
			return tag, false
		}
	}
	return strings.ToLower(field.Name), false
}

func appendProviderConfigPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

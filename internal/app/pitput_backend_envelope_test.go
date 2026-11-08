package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Match compact_tool_envelope.py's sorted ASCII JSON. This projects diagnostics
// only: even returncode zero never advances read/ACK acceptance.
func backendASCIIJSONString(value string) string {
	var output strings.Builder
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if character < 32 || character > 126 {
				if character <= 0xffff {
					fmt.Fprintf(&output, `\u%04x`, character)
				} else {
					value := character - 0x10000
					fmt.Fprintf(&output, `\u%04x\u%04x`, 0xd800+(value>>10), 0xdc00+(value&0x3ff))
				}
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
	return output.String()
}

func backendCompactEnvelope(stringsObserved []string, callHash string) map[string]any {
	result := map[string]any{"status": "unavailable", "returncode": nil, "stdout_bytes": nil,
		"stdout_sha256": nil, "wrapper_error_class": "none"}
	if len(stringsObserved) == 0 {
		return result
	}
	result["status"] = "unrecognized"
	pattern := regexp.MustCompile(`PITPUT_FIXTURE_V1_(call_[0-9a-f]{24}):([A-Za-z0-9_-]+)`)
	type candidate struct {
		call, token string
		padded      bool
	}
	candidates := []candidate{}
	for _, content := range stringsObserved {
		classification := backendNativeError(content)["class"]
		if classification != "none" && classification != "unclassified" {
			result["wrapper_error_class"] = classification
		}
		for _, match := range pattern.FindAllStringSubmatchIndex(content, -1) {
			call := content[match[2]:match[3]]
			digest := sha256.Sum256([]byte(call))
			if hex.EncodeToString(digest[:]) == callHash {
				candidates = append(candidates, candidate{call, content[match[4]:match[5]], match[1] < len(content) && content[match[1]] == '='})
			}
		}
	}
	if len(candidates) == 0 {
		return result
	}
	if len(candidates) != 1 {
		result["status"] = "ambiguous"
		return result
	}
	result["status"] = "invalid"
	item := candidates[0]
	if item.padded || len(item.token) > (100000*4+2)/3 {
		return result
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(item.token)
	if err != nil || len(raw) > 100000 {
		return result
	}
	var fields map[string]json.RawMessage
	var envelope struct {
		Version    int    `json:"v"`
		Call       string `json:"call_id"`
		Returncode int    `json:"returncode"`
		Stdout     string `json:"stdout"`
	}
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 || json.Unmarshal(raw, &envelope) != nil ||
		envelope.Version != 1 || envelope.Call != item.call || envelope.Returncode < -255 || envelope.Returncode > 255 || len(envelope.Stdout) > 65536 {
		return result
	}
	canonical := fmt.Sprintf(`{"call_id":%s,"returncode":%d,"stdout":%s,"v":1}`, backendASCIIJSONString(envelope.Call), envelope.Returncode, backendASCIIJSONString(envelope.Stdout))
	if string(raw) != canonical || base64.RawURLEncoding.EncodeToString(raw) != item.token {
		return result
	}
	digest := sha256.Sum256([]byte(envelope.Stdout))
	result["status"], result["returncode"] = "observed", envelope.Returncode
	result["stdout_bytes"], result["stdout_sha256"] = len(envelope.Stdout), hex.EncodeToString(digest[:])
	return result
}

func backendToolOutputStrings(node map[string]any) []string {
	observed := []string{}
	bytes, visits := 0, 0
	var inspect func(any)
	inspect = func(value any) {
		visits++
		if visits > 256 || len(observed) >= 32 || bytes > 250000 {
			return
		}
		switch value := value.(type) {
		case string:
			bytes += len(value)
			if bytes <= 250000 {
				observed = append(observed, value)
			}
		case []any:
			for _, child := range value {
				inspect(child)
			}
		case map[string]any:
			for _, key := range []string{"output", "stdout", "stderr", "result", "content", "text"} {
				if child, exists := value[key]; exists {
					inspect(child)
				}
			}
		}
	}
	inspect(node)
	if state, ok := node["state"].(map[string]any); ok {
		inspect(state)
	}
	return observed
}

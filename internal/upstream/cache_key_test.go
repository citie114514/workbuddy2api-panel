package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func extractCacheKey(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

func isUUIDv4Shape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return s[14] == '4' && strings.ContainsRune("89ab", rune(s[19]))
}

func TestInjectPromptCacheKey_PreservesExisting(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-set-key"}`
	out := InjectPromptCacheKey([]byte(in), "user-abc-123", "conv-xyz")
	if got := extractCacheKey(t, out); got != "client-set-key" {
		t.Fatalf("existing prompt_cache_key overwritten: got=%q", got)
	}
}

func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`
	got := extractCacheKey(t, InjectPromptCacheKey([]byte(in), uid, ""))
	if !isUUIDv4Shape(got) {
		t.Fatalf("expected UUID v4 shape key, got=%q", got)
	}
	if strings.Contains(got, "user-abc") {
		t.Fatalf("key must not contain raw uid, got=%q", got)
	}
	if again := extractCacheKey(t, InjectPromptCacheKey([]byte(in), uid, "")); again != got {
		t.Fatalf("same conversation produced different keys: %q vs %q", got, again)
	}
	inB := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-99"}`
	if other := extractCacheKey(t, InjectPromptCacheKey([]byte(inB), uid, "")); other == got {
		t.Fatalf("different conversations produced the same key: %q", got)
	}
}

func TestInjectPromptCacheKey_GeneratesStableKey(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	a := extractCacheKey(t, InjectPromptCacheKey(in, "user-abc-123", "conv-stable"))
	b := extractCacheKey(t, InjectPromptCacheKey(in, "user-abc-123", "conv-stable"))
	if a != b {
		t.Fatalf("same input+uid produced different keys: %q vs %q", a, b)
	}
}

func TestInjectPromptCacheKey_AccountIsolation(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	ka := extractCacheKey(t, InjectPromptCacheKey(in, "user-aaa-111", "conv-shared"))
	kb := extractCacheKey(t, InjectPromptCacheKey(in, "user-bbb-222", "conv-shared"))
	if ka == kb {
		t.Fatalf("cross-account key collision: both=%q", ka)
	}
	if !isUUIDv4Shape(ka) || !isUUIDv4Shape(kb) {
		t.Fatalf("keys must be UUID v4 shape: a=%q b=%q", ka, kb)
	}
}

func TestInjectPromptCacheKey_FormatWithoutRawUID(t *testing.T) {
	uid := "1234567890abcdef"
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	got := extractCacheKey(t, InjectPromptCacheKey(in, uid, "conv-fmt"))
	if !isUUIDv4Shape(got) {
		t.Fatalf("expected UUID v4 shape key, got=%q", got)
	}
	if strings.Contains(got, "12345678") {
		t.Fatalf("key contains raw uid prefix: %q", got)
	}
}

func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	got := extractCacheKey(t, InjectPromptCacheKey(in, "user-abc-123", ""))
	if !isUUIDv4Shape(got) {
		t.Fatalf("expected non-empty UUID v4 key, got=%q", got)
	}
}

func TestPrepareBodyOptNoCacheKeyInjection(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[]}`)
	out := PrepareBodyOpt(in, false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := obj["prompt_cache_key"]; present {
		t.Fatalf("PrepareBodyOpt must not inject prompt_cache_key: %v", obj)
	}
}

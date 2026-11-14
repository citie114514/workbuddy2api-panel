package upstream

import (
	"encoding/json"
	"testing"
)

// TestApplyChatDefaults 补齐两个客户端形态字段；显式给出的不覆盖。
func TestApplyChatDefaults(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantRet   string
		wantSumry string // 空串表示不应出现
	}{
		{
			name:    "两个都缺（带思考）",
			in:      `{"model":"fast-model","reasoning_effort":"medium","messages":[]}`,
			wantRet: "long", wantSumry: "auto",
		},
		{
			name:    "两个都缺（无思考）",
			in:      `{"model":"wb-lite","messages":[]}`,
			wantRet: "long",
		},
		{
			name:    "已有 cache_retention 不覆盖",
			in:      `{"cache_retention":"short","reasoning_effort":"low"}`,
			wantRet: "short", wantSumry: "auto",
		},
		{
			name:      "已有 reasoning_summary 不覆盖",
			in:        `{"reasoning_effort":"high","reasoning_summary":"detailed"}`,
			wantRet:   "long",
			wantSumry: "detailed",
		},
		{
			name:    "嵌套 reasoning.effort 也算带思考",
			in:      `{"reasoning":{"effort":"low"}}`,
			wantRet: "long", wantSumry: "auto",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := applyChatDefaults([]byte(tc.in))
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got, _ := obj["cache_retention"].(string); got != tc.wantRet {
				t.Errorf("cache_retention = %q want %q", got, tc.wantRet)
			}
			got, has := obj["reasoning_summary"].(string)
			if tc.wantSumry == "" {
				if has {
					t.Errorf("不该出现 reasoning_summary，实际 %q", got)
				}
			} else if got != tc.wantSumry {
				t.Errorf("reasoning_summary = %q want %q", got, tc.wantSumry)
			}
		})
	}
}

// TestApplyChatDefaultsLeavesNonObject 非 JSON / 非对象负载原样返回。
func TestApplyChatDefaultsLeavesNonObject(t *testing.T) {
	for _, in := range []string{"", "not json", `[{"a":1}]`, `"str"`} {
		if out := applyChatDefaults([]byte(in)); string(out) != in {
			t.Errorf("输入 %q 被改动为 %q", in, out)
		}
	}
}

// TestApplyChatDefaultsPreservesPayload 只加字段，不动原有内容。
func TestApplyChatDefaultsPreservesPayload(t *testing.T) {
	in := `{"model":"fast-model","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`
	out := applyChatDefaults([]byte(in))
	var want, got map[string]any
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if !jsonEqual(v, got[k]) {
			t.Errorf("字段 %s 被改动: %v -> %v", k, v, got[k])
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

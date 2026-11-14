package upstream

import (
	"os"
	"strings"
	"testing"
)

// 自动任务与缓存键是网关自行构造上行标识的两条路径，不得泄露项目前缀或账号明文。
func TestGeneratedOutboundIDsHaveNoSelfIdentifyingPrefix(t *testing.T) {
	for _, path := range []string{"../panel/autotask.go", "cache_key.go"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, `"wb2api-`) || strings.Contains(line, `"wb2a-`) {
				t.Errorf("%s:%d contains a self-identifying ID prefix", path, i+1)
			}
		}
	}
}

func TestTaskOutboundIDsHaveNoSelfIdentifyingPrefix(t *testing.T) {
	tests := map[string][]string{
		"../scheduler/scheduler.go": {`"wb2api-%d"`},
		"../scheduler/travel.go":    {`"wb2api-adopt-%d"`},
		"blackcat.go":               {`"wb2api-night-%d-%d"`},
		"desktop.go":                {`"wb2api-conv-%d"`},
		"school.go":                 {`"wb2api-" + clientToken()`, `"wb2api-mp-pb-" + clientToken()`},
	}
	for path, forbidden := range tests {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range forbidden {
			if strings.Contains(string(raw), id) {
				t.Errorf("%s contains self-identifying ID expression %s", path, id)
			}
		}
	}
}

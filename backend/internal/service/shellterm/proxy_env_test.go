package shellterm

import "testing"

func TestMergeShellProxyEnvAcceptsOnlyProxyKeys(t *testing.T) {
	base := map[string]string{"PATH": `C:\ao`}
	merged, err := mergeShellProxyEnv(base, map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:7890", "NO_PROXY": "127.0.0.1,localhost",
	})
	if err != nil {
		t.Fatalf("proxy env rejected: %v", err)
	}
	if merged["PATH"] != `C:\ao` || merged["HTTPS_PROXY"] != "http://127.0.0.1:7890" || merged["NO_PROXY"] == "" {
		t.Fatalf("unexpected merged env: %#v", merged)
	}
	if base["HTTPS_PROXY"] != "" {
		t.Fatal("base env must not be mutated")
	}
	for _, bad := range []map[string]string{
		{"PATH": `C:\evil`},
		{"HTTPS_PROXY": "http://proxy\r\nPATH=x"},
		{"ALL_PROXY": string(make([]byte, 2049))},
	} {
		if _, err := mergeShellProxyEnv(base, bad); err == nil {
			t.Fatalf("expected rejection for %#v", bad)
		}
	}
	if same, err := mergeShellProxyEnv(base, nil); err != nil || same["PATH"] != `C:\ao` {
		t.Fatalf("empty extra env should keep base: %#v %v", same, err)
	}
}

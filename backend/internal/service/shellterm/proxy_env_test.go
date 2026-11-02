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

// Coding Tools opens agy with every proxy variable (upper and lower case, NO_PROXY,
// NODE_USE_ENV_PROXY) and SSH_CLIENT; that full set was refused as "too many" values.
func TestMergeShellProxyEnvAcceptsTheAgyTerminalEnv(t *testing.T) {
	proxy := "http://127.0.0.1:7890"
	agy := map[string]string{
		"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "ALL_PROXY": proxy, "NO_PROXY": "127.0.0.1,localhost",
		"http_proxy": proxy, "https_proxy": proxy, "all_proxy": proxy, "no_proxy": "127.0.0.1,localhost",
		"NODE_USE_ENV_PROXY": "1", "SSH_CLIENT": "coding-tools",
	}
	merged, err := mergeShellProxyEnv(map[string]string{"PATH": `C:\ao`}, agy)
	if err != nil {
		t.Fatalf("agy terminal env rejected: %v", err)
	}
	if merged["SSH_CLIENT"] != "coding-tools" || merged["https_proxy"] != proxy || merged["PATH"] != `C:\ao` {
		t.Fatalf("unexpected merged env: %#v", merged)
	}
	for _, bad := range []map[string]string{
		{"SSH_CLIENT": "x\r\nPATH=y"},
		{"SSH_CONNECTION": "1.2.3.4 22 5.6.7.8 22"},
	} {
		if _, err := mergeShellProxyEnv(nil, bad); err == nil {
			t.Fatalf("expected rejection for %#v", bad)
		}
	}
}

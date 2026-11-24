package linkpreview

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseHeadSkipsScriptBodies(t *testing.T) {
	doc := []byte(`<html><head>
<script>
const fake = '<meta property="og:title" content="Script Title">';
document.write('<meta property="og:description" content="Script Description">');
</script>
<meta property="og:title" content="Real Title">
<meta property="og:description" content="Real Description">
</head><body></body></html>`)

	m := parseHead(doc)
	if m.ogTitle != "Real Title" {
		t.Errorf("ogTitle = %q, want the tag after the script body to win", m.ogTitle)
	}
	if m.ogDescription != "Real Description" {
		t.Errorf("ogDescription = %q, want the tag after the script body to win", m.ogDescription)
	}
}

func TestParseHeadSkipsStyleBodies(t *testing.T) {
	doc := []byte(`<html><head>
<style>
.x::before { content: '<meta property="og:title" content="Style Title">'; }
</style>
<meta property="og:title" content="Real Title">
</head><body></body></html>`)

	m := parseHead(doc)
	if m.ogTitle != "Real Title" {
		t.Errorf("ogTitle = %q, want the tag after the style body to win", m.ogTitle)
	}
}

func TestParseHeadUnterminatedCommentDoesNotSwallowRest(t *testing.T) {
	doc := []byte(`<html><head>
<!-- a comment that never closes
<meta property="og:title" content="Still Found">
<meta name="description" content="Also Found">
</head><body></body></html>`)

	m := parseHead(doc)
	if m.ogTitle != "Still Found" {
		t.Errorf("ogTitle = %q, want tags after an unterminated comment to be extracted", m.ogTitle)
	}
	if m.description != "Also Found" {
		t.Errorf("description = %q, want tags after an unterminated comment to be extracted", m.description)
	}
}

func TestParseHeadUnterminatedScriptSkipsToEnd(t *testing.T) {
	doc := []byte(`<html><head>
<meta property="og:title" content="Before Script">
<script>const s = '<meta property="og:description" content="Inside Script">';`)

	m := parseHead(doc)
	if m.ogTitle != "Before Script" {
		t.Errorf("ogTitle = %q", m.ogTitle)
	}
	if m.ogDescription != "" {
		t.Errorf("ogDescription = %q, want script content ignored even without a close tag", m.ogDescription)
	}
}

func TestPreviewConcurrentSameURLFetchesOnce(t *testing.T) {
	hits := &atomic.Int32{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// Slow enough that every concurrent caller overlaps the first fetch.
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><title>T</title></head><body></body></html>`)
	}))
	defer origin.Close()

	svc := New(origin.Client())
	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Preview(context.Background(), origin.URL+"/page")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("origin hits = %d, want 1 (singleflight dedup)", got)
	}
}

// Bug hunt 2026-11-24 A4: the shared fetch ran under the first caller's context and its outcome
// was cached whatever ended it. A first hover that moved off the link (its request cancelled)
// cached a 2-minute failure, so every hover of that URL for the next two minutes failed too.
func TestPreviewCancelledFirstHoverDoesNotCacheAFailure(t *testing.T) {
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><title>T</title></head><body></body></html>`)
	}))
	defer origin.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	svc := New(origin.Client())
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := svc.Preview(ctx, origin.URL+"/page"); first <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("a cancelled hover returned a preview")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled hover kept waiting for the shared fetch")
	}
	close(release)
	if _, err := svc.Preview(context.Background(), origin.URL+"/page"); err != nil {
		t.Fatalf("the next hover got %v; a cancelled hover must not cache a failure", err)
	}
}

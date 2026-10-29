package offsite

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// fakeS3 is a bucket in memory that checks each request is signed.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	paths   []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.Host+r.URL.Path)
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKTEST/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
		w.WriteHeader(403)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>unsigned</Message></Error>`)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bkt/")
	switch {
	case r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		type obj struct{ Key string }
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []obj
		}
		for _, k := range keys {
			res.Contents = append(res.Contents, obj{k})
		}
		xml.NewEncoder(w).Encode(res)
	case r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
	case r.Method == "GET":
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Write(b)
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	}
}

func TestS3(t *testing.T) {
	f := &fakeS3{objects: map[string][]byte{"pob/other/deeper.json": nil, "elsewhere.json": nil}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &S3{Endpoint: srv.URL, Region: "garage", Bucket: "bkt", Prefix: "/pob/", PathStyle: true, AccessKey: "AKTEST", SecretKey: "secret"}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Put(ctx, "a.json", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "b.json", []byte("two")); err != nil {
		t.Fatal(err)
	}
	names, err := c.List(ctx)
	if err != nil || strings.Join(names, ",") != "a.json,b.json" {
		t.Fatalf("list: %v %v", names, err)
	}
	if b, err := c.Get(ctx, "b.json"); err != nil || string(b) != "two" {
		t.Fatalf("get: %q %v", b, err)
	}
	if err := c.Delete(ctx, "a.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "a.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted: %v", err)
	}
	if _, ok := f.objects["pob/b.json"]; !ok {
		t.Errorf("stored as %v", f.objects)
	}

	bad := *c
	bad.AccessKey = "WRONG"
	if _, err := bad.List(ctx); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("bad key: %v", err)
	}
	for _, p := range []string{"../x", "a b"} {
		bad := *c
		bad.Prefix = p
		if bad.Check() == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
	if err := c.Put(ctx, "../escape", nil); err == nil {
		t.Error("a name with a path was accepted")
	}
}

func TestS3VirtualHostAndAWS(t *testing.T) {
	c := &S3{Endpoint: "https://s3.example.com", Bucket: "bkt"}
	if u := c.url("pob/a.json", nil).String(); u != "https://bkt.s3.example.com/pob/a.json" {
		t.Errorf("virtual host: %s", u)
	}
	c = &S3{Region: "eu-west-1", Bucket: "bkt"}
	if u := c.url("a.json", nil).String(); u != "https://bkt.s3.eu-west-1.amazonaws.com/a.json" {
		t.Errorf("aws: %s", u)
	}
}

func TestFolder(t *testing.T) {
	f := Folder{Dir: t.TempDir()}
	ctx := context.Background()
	if err := f.Put(ctx, "a.json", []byte("x")); err != nil {
		t.Fatal(err)
	}
	names, _ := f.List(ctx)
	if len(names) != 1 || names[0] != "a.json" {
		t.Errorf("list: %v", names)
	}
	if _, err := f.Get(ctx, "missing.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	for _, n := range []string{"../a", "sub/a", ".hidden"} {
		if f.Put(ctx, n, nil) == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	if err := f.Delete(ctx, "a.json"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "a.json"); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
}

// The signature must match the AWS SDK's own signer (used only here, in a
// test, so the app itself links no AWS code).
func TestSignMatchesAWS(t *testing.T) {
	when := time.Date(2026, 10, 29, 13, 5, 9, 0, time.UTC)
	for _, raw := range []string{
		"http://garage:3900/pob-backups/?list-type=2&prefix=proof-of-basis%2F",
		"https://bkt.s3.eu-west-1.amazonaws.com/?list-type=2&prefix=a%2Fb&continuation-token=1%2BxY%2Fz%3D%3D",
		"http://127.0.0.1:9100/pob-backups/proof-of-basis/proof-of-basis-20261029-130509Z-0123456789abcdef.json",
	} {
		const hash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		mine, _ := http.NewRequest("GET", raw, nil)
		sign(mine, hash, "AKTEST", "secret", "eu-west-1", when)
		theirs, _ := http.NewRequest("GET", raw, nil)
		theirs.Header.Set("X-Amz-Content-Sha256", hash)
		s := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		if err := s.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "AKTEST", SecretAccessKey: "secret"}, theirs, hash, "s3", "eu-west-1", when); err != nil {
			t.Fatal(err)
		}
		if a, b := mine.Header.Get("Authorization"), theirs.Header.Get("Authorization"); a != b {
			t.Errorf("%s\n mine:   %s\n theirs: %s", raw, a, b)
		}
	}
}

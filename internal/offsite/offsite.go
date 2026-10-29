// Package offsite stores backup files somewhere other than the app's own
// disk: an S3-compatible bucket (AWS, Backblaze B2, Wasabi, Cloudflare R2,
// or a self-hosted MinIO or Garage) or a folder, such as a network share
// mounted into the container.
package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// A Target holds files by name, in one flat folder.
type Target interface {
	List(ctx context.Context) ([]string, error)
	Put(ctx context.Context, name string, body []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, name string) error
}

// ErrNotFound is returned by Get for a file that isn't there.
var ErrNotFound = errors.New("not found")

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func checkName(name string) error {
	if !safeName.MatchString(name) {
		return fmt.Errorf("bad file name %q", name)
	}
	return nil
}

// Folder keeps files in a directory.
type Folder struct{ Dir string }

func (f Folder) List(ctx context.Context) ([]string, error) {
	ents, err := os.ReadDir(f.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Put writes to a temporary file first, so a half-written backup never
// takes the real name.
func (f Folder) Put(ctx context.Context, name string, body []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.Dir, ".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(f.Dir, name))
}

func (f Folder) Get(ctx context.Context, name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (f Folder) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(f.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// S3 talks to an S3-compatible bucket with plain HTTP requests signed with
// AWS Signature Version 4. Only the basic object calls are used, so it
// works with self-hosted stores that implement a subset of the API.
type S3 struct {
	Endpoint  string // for example http://minio:9000; empty means AWS itself
	Region    string
	Bucket    string
	Prefix    string // a folder inside the bucket, for example proof-of-basis
	PathStyle bool   // bucket in the path rather than the host name (MinIO, Garage)
	AccessKey string
	SecretKey string
	Client    *http.Client
}

var safePrefix = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)

// Check reports a setting that can't work, before any request is made.
func (c *S3) Check() error {
	if c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("the bucket, access key and secret key are all needed")
	}
	if !safeName.MatchString(c.Bucket) {
		return fmt.Errorf("bad bucket name %q", c.Bucket)
	}
	if !safePrefix.MatchString(c.Prefix) || strings.Contains(c.Prefix, "..") {
		return errors.New("the folder may use only letters, digits and . _ - /")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("the endpoint should look like https://s3.example.com or http://minio:9000")
		}
	}
	return nil
}

func (c *S3) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c *S3) prefix() string {
	p := strings.Trim(c.Prefix, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// url builds the address of a key ("" for the bucket itself).
func (c *S3) url(key string, q url.Values) *url.URL {
	u := &url.URL{Scheme: "https", Host: "s3." + c.region() + ".amazonaws.com"}
	if c.Endpoint != "" {
		e, _ := url.Parse(c.Endpoint)
		u.Scheme, u.Host = e.Scheme, e.Host
		if p := strings.TrimRight(e.Path, "/"); p != "" {
			u.Path = p
		}
	}
	if c.PathStyle {
		u.Path += "/" + c.Bucket + "/" + key
	} else {
		u.Host = c.Bucket + "." + u.Host
		u.Path += "/" + key
	}
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u
}

func (c *S3) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: c.AccessKey, SecretAccessKey: c.SecretKey}
	if err := signer.SignHTTP(ctx, creds, req, hash, "s3", c.region(), time.Now()); err != nil {
		return nil, err
	}
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return resp, s3Error(resp)
	}
	return resp, nil
}

// s3Error turns the XML error body into a short message.
func s3Error(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Code    string
		Message string
	}
	xml.Unmarshal(b, &e)
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Errorf("the storage said %s: %s (HTTP %d)", e.Code, e.Message, resp.StatusCode)
	case e.Code != "":
		return fmt.Errorf("the storage said %s (HTTP %d)", e.Code, resp.StatusCode)
	}
	return fmt.Errorf("the storage answered HTTP %d", resp.StatusCode)
}

func (c *S3) List(ctx context.Context) ([]string, error) {
	var out []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {c.prefix()}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, c.url("", q), nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents              []struct{ Key string }
			IsTruncated           bool
			NextContinuationToken string
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading the bucket's list: %w", err)
		}
		for _, o := range page.Contents {
			name := strings.TrimPrefix(o.Key, c.prefix())
			if name != "" && !strings.Contains(name, "/") {
				out = append(out, name)
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Strings(out)
	return out, nil
}

func (c *S3) Put(ctx context.Context, name string, body []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	if body == nil {
		body = []byte{}
	}
	resp, err := c.do(ctx, http.MethodPut, c.url(c.prefix()+name, nil), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *S3) Get(ctx context.Context, name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, c.url(c.prefix()+name, nil), nil)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<30))
}

func (c *S3) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, c.url(c.prefix()+name, nil), nil)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

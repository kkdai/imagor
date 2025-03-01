package imagor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cshum/imagor/imagorpath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func jsonStr(v interface{}) string {
	buf, _ := json.Marshal(v)
	return string(buf)
}

type loaderFunc func(r *http.Request, image string) (blob *Blob, err error)

func (f loaderFunc) Get(r *http.Request, image string) (*Blob, error) {
	return f(r, image)
}

type saverFunc func(ctx context.Context, image string, blob *Blob) error

func (f saverFunc) Get(r *http.Request, image string) (*Blob, error) {
	// dummy
	return nil, ErrNotFound
}

func (f saverFunc) Stat(ctx context.Context, image string) (*Stat, error) {
	// dummy
	return nil, ErrNotFound
}

func (f saverFunc) Delete(ctx context.Context, image string) error {
	// dummy
	return nil
}

func (f saverFunc) Put(ctx context.Context, image string, blob *Blob) error {
	return f(ctx, image, blob)
}

type processorFunc func(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error)

func (f processorFunc) Process(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error) {
	return f(ctx, blob, p, load)
}
func (f processorFunc) Startup(_ context.Context) error {
	return nil
}
func (f processorFunc) Shutdown(_ context.Context) error {
	return nil
}

func TestWithUnsafe(t *testing.T) {
	logger := zap.NewExample()
	app := New(
		WithUnsafe(true),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			return NewBlobFromBytes([]byte("foo")), nil
		})),
		WithLogger(logger))
	assert.Equal(t, false, app.Debug)
	assert.Equal(t, logger, app.Logger)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo.jpg", nil))
	assert.Equal(t, 200, w.Code)

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "https://example.com/unsafe/foo.jpg", nil))
	assert.Equal(t, 405, w.Code)
	assert.Equal(t, "", w.Body.String())

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))
}

func TestSuppressDeadlockResolve(t *testing.T) {
	ctx := context.Background()
	app := New()
	f, err := app.suppress(ctx, "a", func(ctx context.Context, _ func(*Blob, error)) (*Blob, error) {
		return app.suppress(ctx, "b", func(ctx context.Context, _ func(*Blob, error)) (*Blob, error) {
			return app.suppress(ctx, "a", func(ctx context.Context, _ func(*Blob, error)) (*Blob, error) {
				return NewEmptyBlob(), nil
			})
		})
	})
	assert.Equal(t, NewEmptyBlob(), f)
	require.NoError(t, err)
}

func TestSuppressTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*10)
	defer cancel()
	app := New()
	f, err := app.suppress(ctx, "a", func(ctx context.Context, _ func(*Blob, error)) (*Blob, error) {
		time.Sleep(time.Second)
		return &Blob{}, nil
	})
	assert.Nil(t, f)
	assert.Equal(t, context.DeadlineExceeded, err)
}

func TestSuppressForgetCanceled(t *testing.T) {
	n := 10
	app := New()
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := app.suppress(context.Background(), "a", func(ctx context.Context, _ func(*Blob, error)) (*Blob, error) {
				time.Sleep(time.Millisecond)
				return NewEmptyBlob(), nil
			})
			assert.Nil(t, err)
		}()
		go func() {
			defer wg.Done()
			_, _ = app.suppress(context.Background(), "a", func(ctx context.Context, _ func(*Blob, error)) (*Blob, error) {
				time.Sleep(time.Millisecond)
				return nil, context.Canceled
			})
		}()
	}
	wg.Wait()
}

func TestWithSigner(t *testing.T) {
	app := New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			return NewBlobFromBytes([]byte("foo")), nil
		})),
		WithSigner(imagorpath.NewDefaultSigner("1234")))
	assert.Equal(t, true, app.Debug)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/_-19cQt1szHeUV0WyWFntvTImDI=/foo.jpg", nil))
	assert.Equal(t, 200, w.Code)

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/_-19cQt1szHeUV0WyWFntvTIm/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))
}

func TestWithCustomSigner(t *testing.T) {
	app := New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			return NewBlobFromBytes([]byte("foo")), nil
		})),
		WithSigner(imagorpath.NewHMACSigner(sha256.New, 40, "1234")))
	assert.Equal(t, true, app.Debug)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/91DBDJtTFePFnbaj5Qq8JLvq5sM5VTipE685f4Gp/foo.jpg", nil))
	assert.Equal(t, 200, w.Code)

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/_-19cQt1szHeUV0WyWFntvTImDI=/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))
}

func TestNewBlobFromPathNotFound(t *testing.T) {
	loader := loaderFunc(func(r *http.Request, image string) (*Blob, error) {
		return NewBlobFromFile("./non-exists-path"), nil
	})
	app := New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithUnsafe(true),
		WithLoaders(loader))

	r := httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foobar", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, 404, w.Code)
	assert.Equal(t, jsonStr(ErrNotFound), w.Body.String())

	app = New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithUnsafe(true),
		WithDisableErrorBody(true),
		WithLoaders(loader))

	r = httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foobar", nil)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, 404, w.Code)
	assert.Empty(t, w.Body.String())
}

func TestWithDisableErrorBody(t *testing.T) {
	app := New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithDisableErrorBody(true),
		WithSigner(imagorpath.NewDefaultSigner("1234")))
	assert.True(t, app.DisableErrorBody)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Empty(t, w.Body.String())
}

func TestWithCacheHeaderTTL(t *testing.T) {
	loader := loaderFunc(func(r *http.Request, image string) (blob *Blob, err error) {
		return NewBlobFromBytes([]byte("ok")), nil
	})
	t.Run("default", func(t *testing.T) {
		app := New(
			WithDebug(true),
			WithLogger(zap.NewExample()),
			WithLoaders(loader),
			WithUnsafe(true))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/foo.jpg", nil))
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, "public, s-maxage=604800, max-age=604800, no-transform, stale-while-revalidate=86400", w.Header().Get("Cache-Control"))
	})
	t.Run("custom ttl swr", func(t *testing.T) {
		app := New(
			WithDebug(true),
			WithLogger(zap.NewExample()),
			WithCacheHeaderSWR(time.Second*167),
			WithCacheHeaderTTL(time.Second*169),
			WithLoaders(loader),
			WithUnsafe(true))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/foo.jpg", nil))
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, "public, s-maxage=169, max-age=169, no-transform, stale-while-revalidate=167", w.Header().Get("Cache-Control"))
	})
	t.Run("custom ttl no swr", func(t *testing.T) {
		app := New(
			WithDebug(true),
			WithLogger(zap.NewExample()),
			WithCacheHeaderSWR(time.Second*169),
			WithCacheHeaderTTL(time.Second*169),
			WithLoaders(loader),
			WithUnsafe(true))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/foo.jpg", nil))
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, "public, s-maxage=169, max-age=169, no-transform", w.Header().Get("Cache-Control"))
	})
	t.Run("no cache", func(t *testing.T) {
		app := New(
			WithDebug(true),
			WithLoaders(loader),
			WithCacheHeaderNoCache(true),
			WithUnsafe(true))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/foo.jpg", nil))
		assert.Equal(t, 200, w.Code)
		assert.NotEmpty(t, w.Header().Get("Expires"))
		assert.Equal(t, "private, no-cache, no-store, must-revalidate", w.Header().Get("Cache-Control"))
	})
}

func TestVersion(t *testing.T) {
	app := New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
	)

	r := httptest.NewRequest(
		http.MethodGet, "https://example.com/", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, fmt.Sprintf(`{"imagor":{"version":"%s"}}`, Version), w.Body.String())
}

func TestWithBasePathRedirect(t *testing.T) {
	app := New(
		WithDebug(true),
		WithBasePathRedirect("https://www.bar.com"),
		WithLogger(zap.NewExample()),
	)
	r := httptest.NewRequest(
		http.MethodGet, "https://foo.com/", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	assert.Equal(t, "https://www.bar.com", w.Header().Get("Location"))
}

func TestParams(t *testing.T) {
	app := New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithSigner(imagorpath.NewDefaultSigner("1234")))

	r := httptest.NewRequest(
		http.MethodGet, "https://example.com/params/_-19cQt1szHeUV0WyWFntvTImDI=/foo.jpg", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	buf, _ := json.MarshalIndent(imagorpath.Parse(r.URL.EscapedPath()), "", "  ")
	assert.Equal(t, string(buf), w.Body.String())

	r = httptest.NewRequest(
		http.MethodGet, "https://example.com/params/foo.jpg", nil)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	buf, _ = json.MarshalIndent(imagorpath.Parse(r.URL.EscapedPath()), "", "  ")
	assert.Equal(t, string(buf), w.Body.String())

	app = New(
		WithDebug(true),
		WithLogger(zap.NewExample()),
		WithDisableParamsEndpoint(true),
		WithSigner(imagorpath.NewDefaultSigner("1234")))
	r = httptest.NewRequest(
		http.MethodGet, "https://example.com/params/_-19cQt1szHeUV0WyWFntvTImDI=/foo.jpg", nil)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Body.String())
}

var clock time.Time

type mapStore struct {
	l       sync.Mutex
	Map     map[string]*Blob
	ModTime map[string]time.Time
	LoadCnt map[string]int
	SaveCnt map[string]int
	DelCnt  map[string]int
}

func newMapStore() *mapStore {
	return &mapStore{
		Map: map[string]*Blob{}, LoadCnt: map[string]int{}, SaveCnt: map[string]int{},
		DelCnt: map[string]int{}, ModTime: map[string]time.Time{},
	}
}

func (s *mapStore) Get(r *http.Request, image string) (*Blob, error) {
	s.l.Lock()
	defer s.l.Unlock()
	buf, ok := s.Map[image]
	if !ok {
		return nil, ErrNotFound
	}
	s.LoadCnt[image] = s.LoadCnt[image] + 1
	return buf, nil
}

func (s *mapStore) Put(ctx context.Context, image string, blob *Blob) error {
	s.l.Lock()
	defer s.l.Unlock()
	clock = clock.Add(1)
	s.Map[image] = blob
	s.SaveCnt[image] = s.SaveCnt[image] + 1
	s.ModTime[image] = clock
	return nil
}

func (s *mapStore) Delete(ctx context.Context, image string) error {
	s.l.Lock()
	defer s.l.Unlock()
	delete(s.Map, image)
	s.DelCnt[image] = s.DelCnt[image] + 1
	return nil
}

func (s *mapStore) Stat(ctx context.Context, image string) (*Stat, error) {
	s.l.Lock()
	defer s.l.Unlock()
	t, ok := s.ModTime[image]
	if !ok {
		return nil, ErrNotFound
	}
	return &Stat{
		ModifiedTime: t,
	}, nil
}

func TestWithLoadersStoragesProcessors(t *testing.T) {
	store := newMapStore()
	resultStore := newMapStore()
	newFakeBlob := func(str string) *Blob {
		return NewBlob(func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader([]byte(str))), 0, nil
		})
	}
	app := New(
		WithDebug(true), WithLogger(zap.NewExample()),
		WithLoaders(
			loaderFunc(func(r *http.Request, image string) (*Blob, error) {
				if image == "foo" {
					return newFakeBlob("bar"), nil
				}
				if image == "bar" {
					return newFakeBlob("foo"), nil
				}
				if image == "ping" {
					return newFakeBlob("pong"), nil
				}
				if image == "empty" {
					return nil, nil
				}
				return nil, ErrNotFound
			}),
			loaderFunc(func(r *http.Request, image string) (*Blob, error) {
				if image == "beep" {
					return newFakeBlob("boop"), nil
				}
				if image == "boom" {
					return nil, errors.New("unexpected error")
				}
				if image == "poop" {
					return newFakeBlob("poop"), nil
				}
				if image == "timeout" {
					return newFakeBlob("timeout"), nil
				}
				if image == "dood" {
					return newFakeBlob("dood"), errors.New("error with value")
				}
				return nil, ErrNotFound
			}),
		),
		WithStorages(
			store,
			saverFunc(func(ctx context.Context, image string, buf *Blob) error {
				time.Sleep(time.Millisecond * 2)
				require.NotEqual(t, "dood", image, "should not save error")
				assert.Error(t, context.DeadlineExceeded, ctx.Err())
				return ctx.Err()
			}),
		),
		WithResultStorages(resultStore),
		WithProcessors(
			processorFunc(func(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error) {
				buf, _ := blob.ReadAll()
				if string(buf) == "bar" {
					return newFakeBlob("tar"), ErrPass
				}
				if string(buf) == "poop" {
					return nil, ErrPass
				}
				if string(buf) == "foo" {
					file, err := load("foo")
					if err != nil {
						return nil, err
					}
					return file, err
				}
				return blob, nil
			}),
			processorFunc(func(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error) {
				buf, _ := blob.ReadAll()
				if string(buf) == "tar" {
					b := newFakeBlob("bark")
					return b, nil
				}
				if string(buf) == "poop" {
					return nil, ErrUnsupportedFormat
				}
				return blob, nil
			}),
		),
		WithSaveTimeout(time.Millisecond),
		WithProcessTimeout(time.Second),
		WithUnsafe(true),
	)
	require.NoError(t, app.Startup(context.Background()))
	assert.Equal(t, time.Second, app.ProcessTimeout)
	assert.Equal(t, time.Millisecond, app.SaveTimeout)
	defer require.NoError(t, app.Shutdown(context.Background()))
	t.Parallel()
	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("ok %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/foo", nil))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, "bark", w.Body.String())

			w = httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/bar", nil))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, "bar", w.Body.String())

			w = httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/ping", nil))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, "pong", w.Body.String())
			time.Sleep(time.Millisecond * 10) // make sure storage reached
			require.NotNil(t, store.Map["ping"])
			buf, err := store.Map["ping"].ReadAll()
			require.NoError(t, err)
			assert.Equal(t, "pong", string(buf))
		})
		t.Run(fmt.Sprintf("not found on empty path %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/", nil))
			assert.Equal(t, 404, w.Code)
			assert.Equal(t, jsonStr(ErrNotFound), w.Body.String())
		})
		t.Run(fmt.Sprintf("empty %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/empty", nil))
			assert.Equal(t, 404, w.Code)
			assert.Equal(t, jsonStr(ErrNotFound), w.Body.String())
			assert.Nil(t, store.Map["empty"])
		})
		t.Run(fmt.Sprintf("not found on pass %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/boooo", nil))
			assert.Equal(t, 404, w.Code)
			assert.Equal(t, jsonStr(ErrNotFound), w.Body.String())
		})
		t.Run(fmt.Sprintf("unexpected error %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/boom", nil))
			assert.Equal(t, 500, w.Code)
			assert.Equal(t, jsonStr(NewError("unexpected error", 500)), w.Body.String())
			assert.Nil(t, store.Map["boom"])
		})
		t.Run(fmt.Sprintf("error with value %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/dood", nil))
			assert.Equal(t, 500, w.Code)
			assert.Equal(t, "dood", w.Body.String())
			assert.Nil(t, store.Map["dood"])
		})
		t.Run(fmt.Sprintf("processor error return original %d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/poop", nil))
			assert.Equal(t, ErrUnsupportedFormat.Code, w.Code)
			assert.Equal(t, "poop", w.Body.String())
			assert.Nil(t, store.Map["poop"])
		})
	}
}

type resultKeyFunc func(p imagorpath.Params) string

func (fn resultKeyFunc) Generate(p imagorpath.Params) string {
	return fn(p)
}

func TestWithResultKey(t *testing.T) {
	store := newMapStore()
	resultStore := newMapStore()
	app := New(
		WithDebug(true), WithLogger(zap.NewExample()),
		WithStorages(store),
		WithResultStorages(resultStore),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			return NewBlobFromBytes([]byte(image)), nil
		})),
		WithResultKey(resultKeyFunc(func(p imagorpath.Params) string {
			return "prefix:" + strings.TrimPrefix(p.Path, "meta/")
		})),
		WithUnsafe(true),
		WithModifiedTimeCheck(true),
	)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo", nil))
	time.Sleep(time.Millisecond * 10) // make sure storage reached
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "foo", w.Body.String())

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo", nil))
	time.Sleep(time.Millisecond * 10) // make sure storage reached
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "foo", w.Body.String())

	assert.Equal(t, 0, store.LoadCnt["foo"])
	assert.Equal(t, 1, store.SaveCnt["foo"])
	assert.Equal(t, 1, resultStore.LoadCnt["prefix:foo"])
	assert.Equal(t, 1, resultStore.SaveCnt["prefix:foo"])
}

func TestClientCancel(t *testing.T) {
	app := New(
		WithDebug(true),
		WithUnsafe(true),
		WithLogger(zap.NewExample()),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			time.Sleep(time.Second)
			return NewBlobFromBytes([]byte(image)), nil
		})),
	)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Millisecond)
			cancel()
		}()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "https://example.com/unsafe/foo", nil).WithContext(ctx)
		app.ServeHTTP(w, r)
		assert.Equal(t, 499, w.Code)
		assert.Empty(t, w.Body.String())
	}
}

func TestWithProcessQueueSize(t *testing.T) {
	n := 20
	conn := 3
	size := 6
	app := New(
		WithDebug(true),
		WithUnsafe(true),
		WithLogger(zap.NewExample()),
		WithProcessQueueSize(int64(size)),
		WithProcessConcurrency(int64(conn)),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			time.Sleep(time.Millisecond * 10) // make sure storage reached
			return NewBlobFromBytes([]byte(image)), nil
		})),
	)
	cnt := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, fmt.Sprintf("https://example.com/unsafe/%d", i), nil))
			//fmt.Println(w.Body.String())
			cnt <- w.Code
		}(i)
	}
	result := map[int]int{}
	for i := 0; i < n; i++ {
		code := <-cnt
		result[code]++
	}
	assert.Equal(t, size+conn, result[200])
	assert.Equal(t, n-size-conn, result[429])
}

func TestWithProcessConcurrency(t *testing.T) {
	n := 5
	app := New(
		WithDebug(true),
		WithUnsafe(true),
		WithLogger(zap.NewExample()),
		WithProcessConcurrency(1),
		WithRequestTimeout(time.Millisecond*13),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			time.Sleep(time.Millisecond * 10) // make sure storage reached
			return NewBlobFromBytes([]byte(image)), nil
		})),
	)
	cnt := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, fmt.Sprintf("https://example.com/unsafe/%d", i), nil))
			cnt <- w.Code
		}(i)
	}
	result := map[int]int{}
	for i := 0; i < n; i++ {
		code := <-cnt
		result[code]++
	}
	assert.Equal(t, 1, result[200])
	assert.Equal(t, 4, result[408])
}

func TestWithModifiedTimeCheck(t *testing.T) {
	store := newMapStore()
	resultStore := newMapStore()
	app := New(
		WithDebug(true), WithLogger(zap.NewExample()),
		WithStorages(store),
		WithResultStorages(resultStore),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			return NewBlobFromBytes([]byte(image)), nil
		})),
		WithUnsafe(true),
		WithModifiedTimeCheck(true),
	)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo", nil))
	time.Sleep(time.Millisecond * 10) // make sure storage reached
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "foo", w.Body.String())
	assert.Equal(t, 0, store.LoadCnt["foo"])
	assert.Equal(t, 1, store.SaveCnt["foo"])
	assert.Equal(t, 0, resultStore.LoadCnt["foo"])
	assert.Equal(t, 1, resultStore.SaveCnt["foo"])

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo", nil))
	time.Sleep(time.Millisecond * 10) // make sure storage reached
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "foo", w.Body.String())
	assert.Equal(t, 0, store.LoadCnt["foo"])
	assert.Equal(t, 1, store.SaveCnt["foo"])
	assert.Equal(t, 1, resultStore.LoadCnt["foo"])
	assert.Equal(t, 1, resultStore.SaveCnt["foo"])

	clock = clock.Add(1)
	store.ModTime["foo"] = clock

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo", nil))
	time.Sleep(time.Millisecond * 10) // make sure storage reached
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, 1, store.LoadCnt["foo"])
	assert.Equal(t, 1, store.SaveCnt["foo"])
	assert.Equal(t, 2, resultStore.LoadCnt["foo"])
	assert.Equal(t, 2, resultStore.SaveCnt["foo"])
}

func TestWithSameStore(t *testing.T) {
	store := newMapStore()
	app := New(
		WithDebug(true), WithLogger(zap.NewExample()),
		WithLoaders(
			store,
			loaderFunc(func(r *http.Request, image string) (*Blob, error) {
				if image == "beep" {
					return NewBlobFromBytes([]byte("boop")), nil
				}
				return nil, ErrNotFound
			}),
		),
		WithStorages(store),
		WithSaveTimeout(time.Millisecond),
		WithProcessTimeout(time.Millisecond*2),
		WithUnsafe(true),
	)
	t.Run("should not save from same store", func(t *testing.T) {
		n := 5
		for i := 0; i < n; i++ {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/beep", nil))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, "boop", w.Body.String())
			time.Sleep(time.Millisecond * 10) // make sure storage reached
		}
		assert.Equal(t, n-1, store.LoadCnt["beep"])
		assert.Equal(t, 1, store.SaveCnt["beep"])
	})
}

func TestBaseParams(t *testing.T) {
	app := New(
		WithDebug(true),
		WithUnsafe(true),
		WithBaseParams("filters:watermark(example.jpg)"),
		WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
			return NewBlobFromBytes([]byte("foo")), nil
		})),
		WithProcessors(processorFunc(func(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error) {
			return NewBlobFromBytes([]byte(p.Path)), nil
		})),
	)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/fit-in/200x0/filters:format(jpg)/abc.png", nil)
	app.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "fit-in/200x0/filters:format(jpg):watermark(example.jpg)/abc.png", w.Body.String())
}

func TestAutoWebP(t *testing.T) {
	factory := func(isAuto bool) *Imagor {
		return New(
			WithDebug(true),
			WithUnsafe(true),
			WithAutoWebP(isAuto),
			WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
				return NewBlobFromBytes([]byte("foo")), nil
			})),
			WithProcessors(processorFunc(func(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error) {
				return NewBlobFromBytes([]byte(p.Path)), nil
			})),
			WithDebug(true))
	}

	t.Run("supported auto img tag not enabled", func(t *testing.T) {
		app := factory(false)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "abc.png")
	})
	t.Run("supported auto img tag", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "filters:format(webp)/abc.png")
	})
	t.Run("supported not image tag auto", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "filters:format(webp)/abc.png")
	})
	t.Run("no supported no auto", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "abc.png")
	})
	t.Run("explicit format no auto", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/filters:format(jpg)/abc.png", nil)
		r.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "filters:format(jpg)/abc.png")
	})
}

func TestAutoAVIF(t *testing.T) {
	factory := func(isAuto bool) *Imagor {
		return New(
			WithDebug(true),
			WithUnsafe(true),
			WithAutoAVIF(isAuto),
			WithLoaders(loaderFunc(func(r *http.Request, image string) (*Blob, error) {
				return NewBlobFromBytes([]byte("foo")), nil
			})),
			WithProcessors(processorFunc(func(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error) {
				return NewBlobFromBytes([]byte(p.Path)), nil
			})),
			WithDebug(true))
	}

	t.Run("supported auto img tag not enabled", func(t *testing.T) {
		app := factory(false)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "abc.png")
	})
	t.Run("supported auto img tag", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "filters:format(avif)/abc.png")
	})
	t.Run("supported not image tag auto", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "filters:format(avif)/abc.png")
	})
	t.Run("no supported no auto", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/abc.png", nil)
		r.Header.Set("Accept", "image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "abc.png")
	})
	t.Run("explicit format no auto", func(t *testing.T) {
		app := factory(true)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/filters:format(jpg)/abc.png", nil)
		r.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		app.ServeHTTP(w, r)
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, w.Body.String(), "filters:format(jpg)/abc.png")
	})
}

func TestWithLoadTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), "sleep") {
			time.Sleep(time.Millisecond * 50)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	loader := loaderFunc(func(r *http.Request, image string) (blob *Blob, err error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, image, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		buf, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return NewBlobFromBytes(buf), err
	})

	tests := []struct {
		name string
		app  *Imagor
	}{
		{
			name: "load timeout",
			app: New(
				WithUnsafe(true),
				WithLoadTimeout(time.Millisecond*10),
				WithLoaders(loader),
			),
		},
		{
			name: "request timeout",
			app: New(
				WithUnsafe(true),
				WithRequestTimeout(time.Millisecond*10),
				WithLoaders(loader),
			),
		},
		{
			name: "load timeout > request timeout",
			app: New(
				WithUnsafe(true),
				WithLoadTimeout(time.Millisecond*10),
				WithRequestTimeout(time.Millisecond*100),
				WithLoaders(loader),
			),
		},
		{
			name: "load timeout < request timeout",
			app: New(
				WithUnsafe(true),
				WithLoadTimeout(time.Millisecond*100),
				WithRequestTimeout(time.Millisecond*10),
				WithLoaders(loader),
			),
		},
	}
	for _, tt := range tests {
		t.Run("ok", func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, fmt.Sprintf("https://example.com/unsafe/%s", ts.URL), nil))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, w.Body.String(), "ok")
		})
		t.Run("timeout", func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, fmt.Sprintf("https://example.com/unsafe/%s/sleep", ts.URL), nil))
			assert.Equal(t, http.StatusRequestTimeout, w.Code)
			assert.Equal(t, w.Body.String(), jsonStr(ErrTimeout))
		})
	}
}

func TestSuppression(t *testing.T) {
	app := New(
		WithDebug(true), WithLogger(zap.NewExample()),
		WithLoaders(
			loaderFunc(func(r *http.Request, image string) (*Blob, error) {
				randBytes := make([]byte, 100)
				rand.Read(randBytes)
				time.Sleep(time.Millisecond * 100)
				return NewBlobFromBytes(randBytes), nil
			}),
		),
		WithUnsafe(true),
	)
	n := 10
	type res struct {
		Name string
		Val  string
	}
	resChan := make(chan res)
	defer close(resChan)
	do := func(image string) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/"+image, nil))
		assert.Equal(t, 200, w.Code)
		resChan <- res{image, w.Body.String()}
	}
	for i := 0; i < n; i++ {
		// should suppress calls so every call of same image must be same value
		// though a and b must be different value
		go do("a")
		go do("b")
	}
	resMap := map[string]string{}
	for i := 0; i < n*2; i++ {
		res := <-resChan
		if val, ok := resMap[res.Name]; ok {
			assert.Equal(t, val, res.Val)
		} else {
			resMap[res.Name] = res.Val
		}
	}
	assert.NotEqual(t, resMap["a"], resMap["b"])
}

package tgspam

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umputun/tg-spam/lib/spamcheck"
)

func BenchmarkLearningCorpus(b *testing.B) {
	root := os.Getenv("TGSPAM_AUDIT_CORPUS")
	if root == "" {
		b.Skip("TGSPAM_AUDIT_CORPUS is not set")
	}
	for _, name := range []string{"neoport", "baltia", "europa"} {
		b.Run(name, func(b *testing.B) {
			data, err := os.ReadFile(filepath.Join(root, name+".json"))
			if err != nil {
				b.Fatal(err)
			}
			var corpus struct{ Spam, Ham, Excluded []string }
			if err = json.Unmarshal(data, &corpus); err != nil {
				b.Fatal(err)
			}
			spam, ham, excluded := strings.Join(corpus.Spam, "\n"), strings.Join(corpus.Ham, "\n"), strings.Join(corpus.Excluded, "\n")
			d := NewDetector(Config{})
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err = d.LoadSamples(strings.NewReader(excluded), []io.Reader{strings.NewReader(spam)}, []io.Reader{strings.NewReader(ham)})
			if err != nil {
				b.Fatal(err)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			runtime.KeepAlive(d)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, err = d.LoadSamples(strings.NewReader(excluded), []io.Reader{strings.NewReader(spam)}, []io.Reader{strings.NewReader(ham)})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(live), "model-live-B")
		})
	}
}

func TestLearningCorpusContention(t *testing.T) {
	root := os.Getenv("TGSPAM_AUDIT_CORPUS")
	if root == "" {
		t.Skip("TGSPAM_AUDIT_CORPUS is not set")
	}
	for _, name := range []string{"neoport", "baltia", "europa"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var corpus struct{ Spam, Ham, Excluded []string }
			if err = json.Unmarshal(data, &corpus); err != nil {
				t.Fatal(err)
			}
			store := &learningStore{rows: map[string]spamClass{}}
			for _, text := range corpus.Spam {
				store.rows[text] = ClassSpam
			}
			for _, text := range corpus.Ham {
				store.rows[text] = ClassHam
			}
			d := NewDetector(Config{MaxAllowedEmoji: -1, SimilarityThreshold: .5, MinSpamProbability: 45})
			_, err = d.LoadSamples(strings.NewReader(strings.Join(corpus.Excluded, "\n")),
				[]io.Reader{strings.NewReader(strings.Join(corpus.Spam, "\n"))},
				[]io.Reader{strings.NewReader(strings.Join(corpus.Ham, "\n"))})
			if err != nil {
				t.Fatal(err)
			}
			d.WithSpamUpdater(learningUpdater{store, ClassSpam})
			d.WithHamUpdater(learningUpdater{store, ClassHam})
			req := spamcheck.Request{Msg: "Проба золота 0.5г", AuthoredText: new(string), CheckOnly: true}
			measure := func(update bool) []time.Duration {
				var wg sync.WaitGroup
				if update {
					wg.Go(func() {
						for range 10 {
							if e := d.UpdateSpam("Есть проба 0.5г"); e != nil {
								t.Error(e)
							}
							time.Sleep(time.Millisecond)
						}
					})
				}
				times := make([]time.Duration, 300)
				for i := range times {
					start := time.Now()
					d.Check(req)
					times[i] = time.Since(start)
				}
				wg.Wait()
				slices.Sort(times)
				return times
			}
			baseline, updating := measure(false), measure(true)
			t.Logf("samples=%d check baseline p95=%s max=%s; with 10 serialized rebuilds p95=%s max=%s",
				len(store.rows), baseline[284], baseline[299], updating[284], updating[299])
		})
	}
}

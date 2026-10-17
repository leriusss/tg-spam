package tgspam

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/umputun/tg-spam/lib/spamcheck"
)

func sampleTokens(readers ...io.Reader) ([]string, error) {
	var tokens []string
	for _, reader := range readers {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if text := strings.Trim(scanner.Text(), " \n\r\t"); text != "" {
				tokens = append(tokens, text)
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read sample model input: %w", err)
		}
	}
	return tokens, nil
}

// SampleSnapshot supplies both classes, including preset samples, from one storage snapshot.
type SampleSnapshot interface {
	Snapshot() (spam, ham []string, err error)
}

func (d *Detector) sampleSnapshot() SampleSnapshot {
	if source, ok := d.spamSamplesUpd.(SampleSnapshot); ok {
		return source
	}
	if source, ok := d.hamSamplesUpd.(SampleSnapshot); ok {
		return source
	}
	return nil
}

func (d *Detector) rebuildSamplesLocked() error {
	source := d.sampleSnapshot()
	if source == nil {
		return nil
	}
	spam, ham, err := source.Snapshot()
	if err != nil {
		return fmt.Errorf("read persisted samples: %w", err)
	}
	excluded := make([]string, 0, len(d.excludedTokens))
	for token := range d.excludedTokens {
		excluded = append(excluded, token)
	}
	_, err = d.loadSamplesLocked(strings.NewReader(strings.Join(excluded, "\n")),
		[]io.Reader{strings.NewReader(strings.Join(spam, "\n"))}, []io.Reader{strings.NewReader(strings.Join(ham, "\n"))})
	return err
}

func (d *Detector) recoverSamples() {
	d.lock.RLock()
	dirty := d.samplesErr != nil
	d.lock.RUnlock()
	if !dirty {
		return
	}
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.samplesErr != nil {
		d.samplesErr = d.rebuildSamplesLocked()
	}
}

func (d *Detector) samplesUnavailable() spamcheck.Response {
	return spamcheck.Response{Name: "sample model unavailable", Details: "persisted samples require reload", Error: d.samplesErr}
}

package diag

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// FileWLAN is a WLAN read from a JSON file: the end-to-end test (lowpath
// -wifi-reading) shows a Wi-Fi card on any PC, cable or not, and the PC's
// real adapter is never touched. The file is read on every call, so a test
// can change it while the app runs; a missing file is a PC without Wi-Fi.
type FileWLAN struct {
	path string
	mu   sync.Mutex
	held int // tunings open now
}

// WiFiReadingFile is the file's content. An error field names a failure:
// "no_wifi", "disconnected", "location", "unsupported", or any other text.
type WiFiReadingFile struct {
	Link        *WiFiLink        `json:"link"`
	LinkError   string           `json:"link_error"`
	Survey      *WiFiSurvey      `json:"survey"`
	SurveyError string           `json:"survey_error"`
	Tuning      *WiFiTuningState `json:"tuning"`
	TuneError   string           `json:"tune_error"`
}

// NewFileWLAN reads the readings at path.
func NewFileWLAN(path string) *FileWLAN { return &FileWLAN{path: path} }

func (f *FileWLAN) read() (WiFiReadingFile, error) {
	var r WiFiReadingFile
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return WiFiReadingFile{LinkError: "no_wifi", SurveyError: "no_wifi", TuneError: "no_wifi"}, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("diag: %s: %w", f.path, err)
	}
	return r, nil
}

func fileErr(s string) error {
	switch s {
	case "":
		return nil
	case "no_wifi":
		return ErrNoWiFi
	case "disconnected":
		return ErrWiFiDisconnected
	case "location":
		return ErrLocationDenied
	case "unsupported":
		return wifiUnsupported{"Wi-Fi"}
	}
	return errors.New(s)
}

func (f *FileWLAN) Link(int) (WiFiLink, error) {
	r, err := f.read()
	switch {
	case err != nil:
		return WiFiLink{}, err
	case fileErr(r.LinkError) != nil:
		return WiFiLink{}, fileErr(r.LinkError)
	case r.Link == nil:
		return WiFiLink{}, ErrNoWiFi
	}
	return *r.Link, nil
}

func (f *FileWLAN) Survey(int) (WiFiSurvey, error) {
	r, err := f.read()
	switch {
	case err != nil:
		return WiFiSurvey{}, err
	case fileErr(r.SurveyError) != nil:
		return WiFiSurvey{}, fileErr(r.SurveyError)
	case r.Survey == nil:
		return WiFiSurvey{}, ErrNoWiFi
	}
	return *r.Survey, nil
}

func (f *FileWLAN) Tune(int) (WiFiTuning, error) {
	r, err := f.read()
	switch {
	case err != nil:
		return nil, err
	case fileErr(r.TuneError) != nil:
		return nil, fileErr(r.TuneError)
	case r.Tuning == nil:
		return nil, ErrNoWiFi
	}
	f.mu.Lock()
	f.held++
	f.mu.Unlock()
	return &fileTuning{f: f, state: *r.Tuning}, nil
}

// Held is how many tunings are open (a boost that didn't revert leaves one).
func (f *FileWLAN) Held() int { f.mu.Lock(); defer f.mu.Unlock(); return f.held }

type fileTuning struct {
	f      *FileWLAN
	mu     sync.Mutex
	state  WiFiTuningState
	closed bool
}

func (t *fileTuning) State() WiFiTuningState   { t.mu.Lock(); defer t.mu.Unlock(); return t.state }
func (t *fileTuning) Refresh() WiFiTuningState { return t.State() }
func (t *fileTuning) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		t.state = WiFiTuningState{}
		t.f.mu.Lock()
		t.f.held--
		t.f.mu.Unlock()
	}
	return nil
}

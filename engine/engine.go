package engine

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	eglog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/fsnotify/fsnotify"
)

type Server interface {
	DoneCmd(path, hash, ttype string, size, ts int64) ([]string, error)
}

const (
	CachedTorrentDir = ".cachedTorrents"
)

var (
	ErrTaskExists    = errors.New("Task already exists")
	ErrWaitListEmpty = errors.New("Wait list empty")
	ErrMaxConnTasks  = errors.New("Max conncurrent task reached")
)

//the Engine Cloud Torrent engine, backed by anacrolix/torrent
type Engine struct {
	sync.RWMutex // race condition on ts,client
	taskMutex    sync.Mutex
	cldServer    Server
	cacheDir     string
	client       *torrent.Client
	closeSync    chan struct{}
	config       Config
	ts           map[string]*Torrent
	TsChanged    chan struct{}
	bttracker    []string
	waitList     *syncList
	//file watcher
	watcher *fsnotify.Watcher
}

func New(s Server) *Engine {
	return &Engine{
		ts:        make(map[string]*Torrent),
		cldServer: s,
		waitList:  NewSyncList(),
		TsChanged: make(chan struct{}, 1),
	}
}

func (e *Engine) Config() Config {
	return e.config
}

func (e *Engine) SetConfig(c Config) {
	e.config = c
}

func (e *Engine) Configure(c *Config) error {
	//recieve config
	if c.IncomingPort <= 0 {
		return fmt.Errorf("Invalid incoming port (%d)", c.IncomingPort)
	}
	if c.ScraperURL == "" {
		c.ScraperURL = defaultScraperURL
	}
	if c.TrackerListURL == "" {
		c.TrackerListURL = defaultTrackerListURL
	}

	e.Lock()
	defer e.Unlock()
	tc := torrent.NewDefaultClientConfig()
	tc.NoDefaultPortForwarding = c.NoDefaultPortForwarding
	tc.DisableUTP = c.DisableUTP
	tc.ListenPort = c.IncomingPort
	tc.DataDir = c.DownloadDirectory
	if c.UseMmap {
		tc.DefaultStorage = storage.NewMMap(tc.DataDir)
	}
	if c.MuteEngineLog {
		tc.Logger = eglog.Discard
	}
	tc.Debug = c.EngineDebug
	tc.NoUpload = !c.EnableUpload
	tc.Seed = c.EnableSeeding
	tc.UploadRateLimiter = c.UploadLimiter()
	tc.DownloadRateLimiter = c.DownloadLimiter()
	tc.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{
		Preferred:        c.ObfsPreferred,
		RequirePreferred: c.ObfsRequirePreferred,
	}
	tc.DisableTrackers = c.DisableTrackers
	tc.DisableIPv6 = c.DisableIPv6
	if c.ProxyURL != "" {
		tc.HTTPProxy = func(*http.Request) (*url.URL, error) {
			return url.Parse(c.ProxyURL)
		}
	}

	tc.EstablishedConnsPerTorrent = c.EstablishedConnsPerTorrent
	tc.HalfOpenConnsPerTorrent = c.HalfOpenConnsPerTorrent
	tc.TotalHalfOpenConns = c.TotalHalfOpenConns

	{
		if e.client != nil {
			// stop all current torrents
			for _, t := range e.client.Torrents() {
				t.Drop()
			}
			e.client.Close()
			close(e.closeSync)
			log.Println("Configure: old client closed")
			e.client = nil
			e.ts = make(map[string]*Torrent)
			time.Sleep(3 * time.Second)
		}

		// runtime reconfigure need to retry while creating client,
		// wait max for 3 * 10 seconds
		var err error
		max := 10
		for max > 0 {
			max--
			e.client, err = torrent.NewClient(tc)
			if err == nil {
				break
			}
			log.Printf("[Configure] error %s\n", err)
			time.Sleep(time.Second * 3)
		}
		if err != nil {
			return err
		}
	}

	e.closeSync = make(chan struct{})
	e.cacheDir = path.Join(c.DownloadDirectory, CachedTorrentDir)
	if st, err := os.Stat(e.cacheDir); errors.Is(err, os.ErrNotExist) || !st.IsDir() {
		os.MkdirAll(e.cacheDir, os.ModePerm)
	}
	e.config = *c
	return nil
}

func (e *Engine) IsConfigred() bool {
	e.RLock()
	defer e.RUnlock()
	return e.client != nil
}

// NewMagnet -> newTorrentBySpec
func (e *Engine) NewMagnet(magnetURI string) error {
	log.Println("[NewMagnet] called: ", magnetURI)
	spec, err := torrent.TorrentSpecFromMagnetUri(magnetURI)
	if err != nil {
		return err
	}
	e.newMagnetCacheFile(magnetURI, spec.InfoHash.HexString())
	return e.newTorrentBySpec(spec, taskMagnet)
}

// NewTorrentByReader -> newTorrentBySpec
func (e *Engine) NewTorrentByReader(r io.Reader) error {
	info, err := metainfo.Load(r)
	if err != nil {
		return err
	}
	spec := torrent.TorrentSpecFromMetaInfo(info)
	e.newTorrentCacheFile(info)
	return e.newTorrentBySpec(spec, taskTorrent)
}

// NewTorrentByFilePath -> newTorrentBySpec
func (e *Engine) NewTorrentByFilePath(path string) error {
	// torrent.TorrentSpecFromMetaInfo may panic if the info is malformed
	defer func() error {
		if r := recover(); r != nil {
			err := fmt.Errorf("Error loading new torrent from file %s: %+v", path, r)
			log.Println(err)
			return err
		}
		return nil
	}()

	info, err := metainfo.LoadFromFile(path)
	if err != nil {
		return err
	}
	e.newTorrentCacheFile(info)
	spec := torrent.TorrentSpecFromMetaInfo(info)
	return e.newTorrentBySpec(spec, taskTorrent)
}

func (e *Engine) isReadyAddTask() bool {
	nowTorrentsLen := len(e.client.Torrents())
	if e.config.MaxConcurrentTask > 0 && nowTorrentsLen >= e.config.MaxConcurrentTask {
		return false
	}
	return true
}

// NewTorrentBySpec -> *Torrent -> addTorrentTask
func (e *Engine) newTorrentBySpec(spec *torrent.TorrentSpec, taskT taskType) error {
	ih := spec.InfoHash.HexString()
	log.Println("[newTorrentBySpec] called ", ih)

	e.taskMutex.Lock()
	defer e.taskMutex.Unlock()
	// whether add as pretasks
	if !e.isReadyAddTask() {
		if !e.isTaskInList(ih) {
			log.Printf("[newTorrentBySpec] reached max task %d, add as pretask: %s %v", e.config.MaxConcurrentTask, ih, taskT)
			e.pushWaitTask(ih, taskT)
		} else {
			log.Printf("[newTorrentBySpec] reached max task %d, task already in tasks: %s %v", e.config.MaxConcurrentTask, ih, taskT)
		}
		e.upsertTorrent(ih, spec.DisplayName, true) // show queueing task
		return ErrMaxConnTasks
	}

	t, _ := e.upsertTorrent(ih, spec.DisplayName, false)
	tt, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return err
	}

	meta := tt.Metainfo()
	if len(e.bttracker) > 0 && (e.config.AlwaysAddTrackers || len(meta.AnnounceList) == 0) {
		log.Printf("[newTorrent] added %d public trackers\n", len(e.bttracker))
		tt.AddTrackers([][]string{e.bttracker})
	}

	go e.torrentEventProcessor(tt, t, ih)
	return nil
}

func (e *Engine) torrentEventProcessor(tt *torrent.Torrent, t *Torrent, ih string) {

	select {
	case <-e.closeSync:
		log.Println("Engine shutdown while waiting Info", ih)
		tt.Drop()
		return
	case <-t.dropWait:
		tt.Drop()
		log.Println("Task Dropped while waiting Info", ih)
		go e.NextWaitTask()
		return
	case <-tt.GotInfo():
		// Already got full torrent info
		// If the origin is from a magnet link, remove it, cache the torrent data
		e.removeMagnetCache(ih)
		m := tt.Metainfo()
		e.newTorrentCacheFile(&m)
		t.updateOnGotInfo(tt)
		e.TsChanged <- struct{}{}
	}

	if e.config.AutoStart {
		go e.StartTorrent(ih)
	}

	timeTk := time.NewTicker(3 * time.Second)
	defer timeTk.Stop()

	// main loop updating the torrent status to our struct
	for {
		select {
		case <-timeTk.C:
			if !t.IsAllFilesDone {
				t.updateFileStatus()
			}
			if !t.Done {
				t.updateTorrentStatus()
			}
			t.updateConnStat()
			e.taskRoutine(t)
		case <-t.dropWait:
			tt.Drop()
			log.Println("Task Droped, exit loop: ", ih)
			go e.NextWaitTask()
			return
		case <-e.closeSync:
			log.Println("Engine shutdown while downloading", ih)
			tt.Drop()
			return
		}
	}
}

//GetTorrents just get the local infohash->Torrent map
func (e *Engine) GetTorrents() map[string]*Torrent {
	return e.ts
}

// TaskRoutine
func (e *Engine) taskRoutine(t *Torrent) {

	// stops task on reaching ratio
	if e.config.SeedRatio > 0 &&
		t.SeedRatio > e.config.SeedRatio &&
		t.Started &&
		!t.ManualStarted &&
		t.Done {
		log.Println("[TaskRoutine] Stopped due to reaching SeedRatio", t.SeedRatio)
		go e.StopTorrent(t.InfoHash)
	}

	// remove task when stopped start not restarted after `RemoveTaskAfterStopped`
	if e.config.RemoveTaskAfterStopped > 0 && e.config.SeedRatio > 0 &&
		t.SeedRatio >= e.config.SeedRatio && !t.Started && t.Done &&
		int(time.Since(t.StartedAt).Seconds()) > e.config.RemoveTaskAfterStopped {
		log.Println("[TaskRoutine] Delete due to reaching SeedRatio", t.SeedRatio)
		go func() {
			e.DeleteTorrent(t.InfoHash)
			e.RemoveCache(t.InfoHash)
		}()
	}

}

func (e *Engine) ManualStartTorrent(infohash string) error {
	if err := e.StartTorrent(infohash); err == nil {
		t, _ := e.getTorrent(infohash)
		t.Lock()
		defer t.Unlock()
		t.ManualStarted = true
	} else {
		return err
	}
	return nil
}

func (e *Engine) StartTorrent(infohash string) error {
	log.Println("StartTorrent ", infohash)
	e.Lock()
	defer e.Unlock()

	t, err := e.getTorrent(infohash)
	if err != nil {
		return err
	}
	t.Lock()
	defer t.Unlock()

	if t.Started {
		return fmt.Errorf("Already started")
	}
	t.Started = true
	t.StartedAt = time.Now()
	for _, f := range t.Files {
		if f != nil {
			f.Started = true
		}
	}
	if t.t.Info() != nil {
		t.t.AllowDataUpload()
		t.t.AllowDataDownload()

		// start all files by setting the priority to normal
		for _, f := range t.t.Files() {
			f.SetPriority(torrent.PiecePriorityNormal)
		}
	}
	return nil
}

func (e *Engine) StopTorrent(infohash string) error {
	log.Println("StopTorrent ", infohash)
	e.Lock()
	defer e.Unlock()
	t, err := e.getTorrent(infohash)
	if err != nil {
		return err
	}
	t.Lock()
	defer t.Unlock()

	if !t.Started {
		return fmt.Errorf("Already stopped")
	}

	if t.t.Info() != nil {
		// stop all files by setting the priority to None
		for _, f := range t.t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}

		t.t.DisallowDataUpload()
		t.t.DisallowDataDownload()
	}

	t.Started = false
	t.StoppedAt = time.Now()
	for _, f := range t.Files {
		if f != nil {
			f.Started = false
		}
	}

	return nil
}

func (e *Engine) DeleteTorrent(infohash string) error {
	log.Println("DeleteTorrent", infohash)
	e.Lock()
	defer e.Unlock()

	t, err := e.getTorrent(infohash)
	if err != nil {
		return err
	}
	close(t.dropWait)
	e.waitList.Remove(infohash)
	e.deleteTorrent(infohash)
	return nil
}

func (e *Engine) StartFile(infohash, filepath string) error {
	t, err := e.getTorrent(infohash)
	if err != nil {
		return err
	}
	t.Lock()
	defer t.Unlock()
	var f *File
	for _, file := range t.Files {
		if file.Path == filepath {
			f = file
			break
		}
	}
	if f == nil {
		return fmt.Errorf("Missing file %s", filepath)
	}
	if f.Started {
		return fmt.Errorf("already started")
	}
	t.Started = true
	f.Started = true
	f.f.SetPriority(torrent.PiecePriorityNormal)
	return nil
}

func (e *Engine) StopFile(infohash, filepath string) error {
	t, err := e.getTorrent(infohash)
	if err != nil {
		return err
	}
	t.Lock()
	defer t.Unlock()
	var f *File
	for _, file := range t.Files {
		if file.Path == filepath {
			f = file
			break
		}
	}
	if f == nil {
		return fmt.Errorf("Missing file %s", filepath)
	}
	if !f.Started {
		return fmt.Errorf("already stopped")
	}
	f.Started = false
	f.f.SetPriority(torrent.PiecePriorityNone)

	allStopped := true
	for _, file := range t.Files {
		if file.Started {
			allStopped = false
			break
		}
	}

	if allStopped {
		go e.StopTorrent(infohash)
	}

	return nil
}

func (e *Engine) RemoveCache(infohash string) {
	e.removeMagnetCache(infohash)
	e.removeTorrentCache(infohash)
}

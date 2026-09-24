package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/source"
)

func (a *app) doctor(args []string) error {
	flags := newFlagSet("doctor")
	if _, err := a.parseFlags(flags, args, "agentory doctor", "Check the environment and index health."); err != nil {
		return err
	}
	failed := 0
	report := func(ok bool, what, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Fprintf(a.stdout, "[%s] %-18s %s\n", mark, what, detail)
	}

	for _, src := range source.All() {
		for _, root := range src.Roots() {
			n, err := countFiles(root, src.Match)
			switch {
			case err != nil:
				report(false, src.Name()+" root", fmt.Sprintf("%s: %v", root, err))
			default:
				report(true, src.Name()+" root", fmt.Sprintf("%s (%d files)", root, n))
			}
		}
	}

	path := index.DefaultPath()
	db, err := index.Open(path)
	if err != nil {
		report(false, "index", err.Error())
		return fmt.Errorf("%d check(s) failed", failed)
	}
	defer db.Close()
	report(true, "index", fmt.Sprintf("%s (%s)", path, humanBytes(dbSize(path))))

	var version string
	db.QueryRow(`SELECT sqlite_version()`).Scan(&version)
	report(version != "", "sqlite", version+" (pure Go, no cgo)")

	var probe int
	err = db.QueryRow(`SELECT count(*) FROM msgs_fts WHERE msgs_fts MATCH '"abc"'`).Scan(&probe)
	report(err == nil, "fts5 trigram", errString(err, "available"))

	report(db.CheckFTS() == nil, "fts consistency", errString(db.CheckFTS(), "msgs_fts matches msgs"))

	var qc string
	db.QueryRow(`PRAGMA quick_check`).Scan(&qc)
	report(qc == "ok", "integrity", qc)

	stale, err := a.staleFiles(db)
	report(err == nil, "freshness", errString(err, fmt.Sprintf("%d file(s) changed since last sync", stale)))

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func errString(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}

func countFiles(root string, match func(string) bool) (int, error) {
	if _, err := os.Stat(root); err != nil {
		return 0, err
	}
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && match(p) {
			n++
		}
		return nil
	})
	return n, err
}

// staleFiles counts files whose size or mtime differs from the index.
func (a *app) staleFiles(db *index.DB) (int, error) {
	known := map[string][2]int64{}
	rows, err := db.Query(`SELECT path, size, mtime FROM files`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var p string
		var s, m int64
		rows.Scan(&p, &s, &m)
		known[p] = [2]int64{s, m}
	}
	rows.Close()
	stale := 0
	for _, src := range source.All() {
		for _, root := range src.Roots() {
			filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !src.Match(p) {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				if k, ok := known[p]; !ok || k[0] != info.Size() || k[1] != info.ModTime().UnixNano() {
					stale++
				}
				return nil
			})
		}
	}
	return stale, nil
}

func (a *app) watch(args []string) error {
	var sources string
	var debounce int
	flags := newFlagSet("watch")
	flags.str(&sources, "", "source", "", "<names>", "comma list of sources")
	flags.int(&debounce, "", "debounce", 1000, "<ms>", "wait this long after the last change before syncing")
	if _, err := a.parseFlags(flags, args, "agentory watch [flags]",
		"Watch the history directories and update the index as files change. Stop with Ctrl-C."); err != nil {
		return err
	}
	srcs, err := source.Select(splitList(sources))
	if err != nil {
		return errUsage{err.Error()}
	}
	db, err := index.Open(index.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	addTree := func(root string) {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				if err := w.Add(p); err != nil {
					fmt.Fprintf(a.stderr, "cannot watch %s: %v\n", p, err)
				}
			}
			return nil
		})
	}
	for _, s := range srcs {
		for _, r := range s.Roots() {
			addTree(r)
		}
	}

	sync := func() {
		st, err := db.Sync(a.ctx, srcs, index.Options{})
		switch {
		case err != nil && a.ctx.Err() == nil:
			fmt.Fprintf(a.stderr, "%s sync error: %v\n", time.Now().Format("15:04:05"), err)
		case err == nil && st.Changed():
			fmt.Fprintf(a.stdout, "%s +%d messages (%d new, %d appended, %d rebuilt files)\n",
				time.Now().Format("15:04:05"), st.Messages, st.New, st.Appended, st.Rebuilt)
		}
	}
	sync()
	fmt.Fprintf(a.stderr, "watching for changes (index %s)…\n", db.Path)

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					addTree(ev.Name)
				}
			}
			timer.Reset(time.Duration(debounce) * time.Millisecond)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(a.stderr, "watch error: %v\n", err)
		case <-timer.C:
			sync()
		}
	}
}

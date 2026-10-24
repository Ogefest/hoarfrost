package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

type queueData struct {
	Storages []string
	State    string
	Counts   map[string]int
	Jobs     []queue.Job
	Page     int
	More     bool
}

func (s *Server) queuePage(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = queue.Queued
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	jobs, err := s.queue.List(r.Context(), state, perPage+1, (page-1)*perPage)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := queueData{State: state, Page: page, More: len(jobs) > perPage, Storages: s.Coord.Storages()}
	if d.More {
		jobs = jobs[:perPage]
	}
	d.Jobs = jobs
	d.Counts, _ = s.queue.Counts(r.Context())
	s.render(w, r, "queue.html", "Queue", d)
}

func (s *Server) queueAdd(w http.ResponseWriter, r *http.Request) {
	var errs []string
	added := 0
	for _, line := range strings.Split(r.PostFormValue("urls"), "\n") {
		u := strings.TrimSpace(line)
		if u == "" {
			continue
		}
		if _, err := s.Coord.AddURL(r.Context(), coordinator.URLRequest{URL: u, Source: r.PostFormValue("source"),
			Engine: r.PostFormValue("engine"), Storage: r.PostFormValue("storage")}); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		added++
	}
	if len(errs) > 0 {
		redirectErr(w, r, "/queue", errors.New(strings.Join(errs, "; ")))
		return
	}
	redirect(w, r, "/queue", fmt.Sprintf("%d URL(s) queued.", added))
}

func (s *Server) queueRetry(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.queue.Retry(r.Context(), id); err != nil {
		redirectErr(w, r, "/queue?state=failed", err)
		return
	}
	redirect(w, r, "/queue?state=failed", "Job requeued.")
}

func (s *Server) queueRetryFailed(w http.ResponseWriter, r *http.Request) {
	n, err := s.queue.RetryAllFailed(r.Context())
	if err != nil {
		redirectErr(w, r, "/queue?state=failed", err)
		return
	}
	redirect(w, r, "/queue", fmt.Sprintf("%d job(s) requeued.", n))
}

// ocr recognizes text of an archived PDF or image and stores it as a new
// metadata record, so the result is part of the archive and reindexable.
func (s *Server) ocr(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pageByID(w, r)
	if !ok {
		return
	}
	to := fmt.Sprintf("/page/%d", p.ID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	rec, err := s.idx.Record(ctx, p.RecordID)
	if err != nil {
		redirectErr(w, r, to, err)
		return
	}
	res, err := s.lib.Fetch(ctx, rec)
	if err != nil {
		redirectErr(w, r, to, err)
		return
	}
	text, err := extract.OCR(ctx, res.Body, res.ContentType, r.PostFormValue("lang"))
	if err != nil {
		redirectErr(w, r, to, err)
		return
	}
	_, err = s.Coord.Record(ctx, p.Storage, "", p.Source, "ocr", func(rr *recorder.Recorder) error {
		_, err := rr.WriteMetadata(recorder.PageInfo{URL: p.URI, RecordID: p.RecordID, Title: p.Title, Text: text,
			Captured: time.Now(), Source: p.Source, Engine: "ocr", Kind: "ocr", Mime: p.Mime, Status: p.Status})
		return err
	})
	if err != nil {
		redirectErr(w, r, to, err)
		return
	}
	redirect(w, r, to, fmt.Sprintf("OCR stored (%d characters).", len([]rune(text))))
}

type systemData struct {
	Storage   []string
	Storages  []string
	Epochs    []index.EpochInfo
	Catalogs  map[string]*archive.Catalog
	Users     []auth.User
	Workers   []coordinator.WorkerStatus
	Current   epoch.ID
	Sizes     map[string]int64
	Task      string
	TaskState string
	TaskLast  string
}

func (s *Server) systemPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := systemData{Storage: s.Storage, Storages: s.Coord.Storages(), Workers: s.Coord.Workers(), Current: epoch.Of(time.Now()), Catalogs: map[string]*archive.Catalog{}}
	var err error
	if d.Epochs, err = s.idx.Epochs(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.SliceStable(d.Epochs, func(i, j int) bool { return d.Epochs[i].Epoch > d.Epochs[j].Epoch })
	d.Sizes = map[string]int64{}
	if sizes, err := s.idx.EpochSizes(ctx); err == nil {
		for _, e := range sizes {
			d.Sizes[e.Storage+"/"+string(e.Epoch)] = e.Bytes
		}
	}
	for _, arc := range s.lib.All() {
		if cats, err := arc.Catalogs(ctx); err == nil {
			for _, c := range cats {
				d.Catalogs[arc.Name+"/"+string(c.Epoch)] = c
			}
		}
	}
	d.Users, _ = s.Auth.Users(ctx)
	d.Task, d.TaskState, d.TaskLast = s.tasks.snapshot()
	s.render(w, r, "system.html", "System", d)
}

func (s *Server) taskStatus(w http.ResponseWriter, r *http.Request) {
	var d systemData
	d.Task, d.TaskState, d.TaskLast = s.tasks.snapshot()
	s.fragment(w, r, "system.html", "task", d)
}

func (s *Server) verifyEpoch(w http.ResponseWriter, r *http.Request) {
	arc, ep, err := s.storageEpoch(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.tasks.start("verification of "+arc.Name+"/"+string(ep), func(progress func(string)) error {
		progress("reading package")
		v, err := arc.Verify(context.Background(), ep)
		if err == nil && !v.OK {
			err = fmt.Errorf("%d problem(s), first: %s: %s", len(v.Problems), v.Problems[0].Path, v.Problems[0].Message)
		}
		return err
	})
	if err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	redirect(w, r, "/system", "Verification started.")
}

func (s *Server) verifyAll(w http.ResponseWriter, r *http.Request) {
	err := s.tasks.start("integrity check", func(progress func(string)) error {
		ctx := context.Background()
		var failed []string
		for _, arc := range s.lib.All() {
			cats, err := arc.Catalogs(ctx)
			if err != nil {
				return err
			}
			for i, c := range cats {
				progress(fmt.Sprintf("%s/%s (%d/%d)", arc.Name, c.Epoch, i+1, len(cats)))
				v, err := arc.Verify(ctx, c.Epoch)
				if err != nil || !v.OK {
					failed = append(failed, arc.Name+"/"+string(c.Epoch))
				}
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("problems in %s", strings.Join(failed, ", "))
		}
		return nil
	})
	if err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	redirect(w, r, "/system", "Integrity check started.")
}

func (s *Server) sealEpoch(w http.ResponseWriter, r *http.Request) {
	arc, ep, err := s.storageEpoch(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	force := r.PostFormValue("force") == "1"
	err = s.tasks.start("sealing of "+arc.Name+"/"+string(ep), func(progress func(string)) error {
		progress("packaging")
		_, err := s.Coord.Seal(context.Background(), arc.Name, ep, force)
		return err
	})
	if err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	redirect(w, r, "/system", "Sealing started.")
}

func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	opts := archive.ReindexOptions{Force: r.PostFormValue("force") == "1"}
	if e := r.PostFormValue("epoch"); e != "" {
		ep, err := epoch.Parse(e)
		if err != nil {
			redirectErr(w, r, "/system", err)
			return
		}
		opts.Epoch = ep
	}
	err := s.tasks.start("reindex", func(progress func(string)) error {
		for _, arc := range s.lib.All() {
			opts.Progress = func(ep epoch.ID, done, total int) { progress(fmt.Sprintf("%s/%s (%d/%d)", arc.Name, ep, done, total)) }
			if _, err := arc.Reindex(context.Background(), opts); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	redirect(w, r, "/system", "Reindex started.")
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Auth.CreateUser(r.Context(), r.PostFormValue("login"), r.PostFormValue("password"), r.PostFormValue("role")); err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	redirect(w, r, "/system", "User created.")
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	if login == userFrom(r).Login {
		redirectErr(w, r, "/system", errors.New("you cannot delete yourself"))
		return
	}
	if err := s.Auth.DeleteUser(r.Context(), login); err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	redirect(w, r, "/system", "User deleted.")
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	if login == userFrom(r).Login {
		redirectErr(w, r, "/system", errors.New("you cannot change your own role"))
		return
	}
	if err := s.Auth.SetRole(r.Context(), login, r.PostFormValue("role")); err != nil {
		redirectErr(w, r, "/system", err)
		return
	}
	if pw := r.PostFormValue("password"); pw != "" {
		if err := s.Auth.SetPassword(r.Context(), login, pw); err != nil {
			redirectErr(w, r, "/system", err)
			return
		}
	}
	redirect(w, r, "/system", "User updated.")
}

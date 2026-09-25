package api

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/corticoide/mockvision/backend/internal/app"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/pkg"
)

// --- Authentication ---

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type meResponse struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

func meFrom(sess app.Session) meResponse {
	var m meResponse
	m.User.ID, m.User.Username, m.User.Role = sess.User.ID, sess.User.Username, string(sess.User.Role)
	m.ExpiresAt = sess.ExpiresAt
	return m
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decode(r, &c); err != nil {
		s.writeError(w, r, err)
		return
	}
	token, sess, err := s.svc.Setup(r.Context(), c.Username, c.Password, clientIP(r), r.UserAgent())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, token, sess.ExpiresAt)
	writeJSON(w, http.StatusCreated, meFrom(sess))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decode(r, &c); err != nil {
		s.writeError(w, r, err)
		return
	}
	token, sess, err := s.svc.Login(r.Context(), c.Username, c.Password, clientIP(r), r.UserAgent())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, token, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, meFrom(sess))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.svc.Logout(r.Context(), c.Value)
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		s.unauthenticated(w, r)
		return
	}
	sess, err := s.svc.Authenticate(r.Context(), c.Value)
	if err != nil {
		s.unauthenticated(w, r)
		return
	}
	writeJSON(w, http.StatusOK, meFrom(sess))
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openAPISpec)
}

// --- Node and settings ---

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Node(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleNodeMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Metrics(r.Context()))
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Settings(r.Context()))
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var p app.SettingsPatch
	if err := decode(r, &p); err != nil {
		s.writeError(w, r, err)
		return
	}
	set, err := s.svc.UpdateSettings(r.Context(), actor(r), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// --- Packages and profiles ---

// readUpload returns the uploaded file of a multipart form (field "file"),
// or the raw body with ?filename=.
func readUpload(w http.ResponseWriter, r *http.Request, limit int64) (string, []byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return "", nil, badReq("invalid multipart body")
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return "", nil, badReq("the form has no file field")
			}
			if err != nil {
				return "", nil, badReq("invalid multipart body")
			}
			if part.FormName() != "file" {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(part, limit+1))
			if err != nil {
				return "", nil, badReq("upload interrupted")
			}
			if int64(len(data)) > limit {
				return "", nil, badReq("the file is too large")
			}
			return part.FileName(), data, nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return "", nil, badReq("upload interrupted")
	}
	if int64(len(data)) > limit {
		return "", nil, badReq("the file is too large")
	}
	return r.URL.Query().Get("filename"), data, nil
}

func (s *Server) handleImportPackage(w http.ResponseWriter, r *http.Request) {
	name, data, err := readUpload(w, r, pkg.MaxPackageBytes)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.svc.ImportPackage(r.Context(), actor(r), name, data)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListProfiles(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func profileRef(r *http.Request) (string, string) {
	return r.PathValue("vendor") + "/" + r.PathValue("model"), r.PathValue("version")
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	id, version := profileRef(r)
	p, err := s.svc.GetProfile(r.Context(), id, version)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleProfileAction(w http.ResponseWriter, r *http.Request) {
	id, version := profileRef(r)
	var archived bool
	switch r.PathValue("action") {
	case "archive":
		archived = true
	case "unarchive":
	default:
		writeProblem(w, r, Problem{Type: problemType + "not-found", Title: "Unknown action", Status: http.StatusNotFound})
		return
	}
	p, err := s.svc.ArchiveProfile(r.Context(), actor(r), id, version, archived)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// --- Cameras ---

func (s *Server) handleListCameras(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListCameras(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleCreateCamera(w http.ResponseWriter, r *http.Request) {
	var in app.CreateCameraInput
	if err := decode(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.svc.CreateCamera(r.Context(), actor(r), in)
	if err != nil && v == nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleGetCamera(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetCamera(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleUpdateCamera(w http.ResponseWriter, r *http.Request) {
	var in app.UpdateCameraInput
	if err := decode(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.svc.UpdateCamera(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteCamera(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.svc.DeleteCamera(ctx, actor(r), r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCameraAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id := r.PathValue("id")
	var (
		v   *app.CameraView
		err error
	)
	switch r.PathValue("action") {
	case "start":
		v, err = s.svc.StartCamera(ctx, actor(r), id)
	case "stop":
		v, err = s.svc.StopCamera(ctx, actor(r), id)
	case "restart":
		v, err = s.svc.RestartCamera(ctx, actor(r), id)
	default:
		writeProblem(w, r, Problem{Type: problemType + "not-found", Title: "Unknown action", Status: http.StatusNotFound,
			Detail: "available actions: start, stop, restart"})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) handleCameraStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetCamera(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": v.Status, "endpoints": v.Endpoints, "metrics": v.Metrics})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	params, err := s.svc.CameraConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"params": params})
}

func (s *Server) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values map[string]any `json:"values"`
	}
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	params, err := s.svc.UpdateCameraConfig(r.Context(), actor(r), r.PathValue("id"), body.Values)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"params": params})
}

func (s *Server) handleCameraMetrics(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-10 * time.Minute)
	if v := r.URL.Query().Get("since"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.writeError(w, r, badReq("since must be Unix milliseconds"))
			return
		}
		since = time.UnixMilli(ms)
	}
	samples, err := s.svc.CameraMetrics(r.Context(), r.PathValue("id"), since)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": samples})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	path, err := s.svc.SnapshotFile(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	serveFile(w, r, path, "image/jpeg")
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	var in app.TriggerInput
	if err := decode(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ev, err := s.svc.TriggerEvent(ctx, actor(r), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// --- Assets ---

func (s *Server) handleListAssets(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListAssets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleUploadAsset(w http.ResponseWriter, r *http.Request) {
	name, data, err := readUpload(w, r, media.MaxImageBytes)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := s.svc.UploadAsset(r.Context(), actor(r), name, bytes.NewReader(data))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleAssetContent(w http.ResponseWriter, r *http.Request) {
	path, mimeType, err := s.svc.AssetFile(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	serveFile(w, r, path, mimeType)
}

func (s *Server) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteAsset(r.Context(), actor(r), r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serveFile(w http.ResponseWriter, r *http.Request, path, mimeType string) {
	f, err := os.Open(path)
	if err != nil {
		writeProblem(w, r, Problem{Type: problemType + "not-found", Title: "Not found", Status: http.StatusNotFound})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeProblem(w, r, Problem{Type: problemType + "not-found", Title: "Not found", Status: http.StatusNotFound})
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, max-age=5")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// --- Targets ---

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListTargets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var in app.TargetInput
	if err := decode(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := s.svc.CreateTarget(r.Context(), actor(r), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.GetTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	var in app.TargetInput
	if err := decode(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := s.svc.UpdateTarget(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteTarget(r.Context(), actor(r), r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.TestTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- Events ---

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.writeError(w, r, badReq("limit must be a positive integer"))
			return
		}
		limit = n
	}
	page, err := s.svc.ListEvents(r.Context(), app.EventFilter{CameraID: q.Get("camera_id"), Type: q.Get("type"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.svc.GetEvent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

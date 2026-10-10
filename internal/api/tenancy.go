package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dseif0x/games-operator/internal/apps"
	"github.com/dseif0x/games-operator/internal/auth"
	"github.com/dseif0x/games-operator/internal/store"
)

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

func validPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 256 {
		return fmt.Errorf("the password must be 8 to 256 characters")
	}
	return nil
}

// ---- users (admin) ----

type userRequest struct {
	Username string       `json:"username"`
	Password string       `json:"password"`
	Role     string       `json:"role"`
	Disabled *bool        `json:"disabled"`
	Quota    *store.Quota `json:"quota"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.Users().List(r.Context())
	if err != nil {
		s.mapErr(w, err)
		return
	}
	out := make([]userJSON, 0, len(list))
	for _, u := range list {
		out = append(out, s.userWithUsage(r.Context(), u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "defaults": map[string]any{"max_apps": s.Apps.Defaults.MaxApps, "max_storage": s.Apps.Defaults.MaxStorage}})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	if !usernameRE.MatchString(req.Username) {
		writeErr(w, http.StatusBadRequest, "username: 2-32 lower-case letters, digits, dot, underscore or dash")
		return
	}
	if err := validPassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	role := req.Role
	if role == "" {
		role = store.RoleUser
	}
	if role != store.RoleUser && role != store.RoleAdmin {
		writeErr(w, http.StatusBadRequest, "role must be user or admin")
		return
	}
	if req.Quota != nil {
		if err := validQuota(*req.Quota); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	u := &store.User{Username: req.Username, PasswordHash: hash, Role: role}
	if req.Quota != nil {
		u.Quota = *req.Quota
	}
	if req.Disabled != nil {
		u.Disabled = *req.Disabled
	}
	if err := s.Store.Users().Create(r.Context(), u); err != nil {
		s.mapErr(w, err)
		return
	}
	s.Log.Info("user created", "user", u.Username, "role", u.Role, "by", principal(r).User.Username)
	writeJSON(w, http.StatusCreated, s.userWithUsage(r.Context(), u))
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	u, err := s.Store.Users().GetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	var req userRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Role != "" {
		if req.Role != store.RoleUser && req.Role != store.RoleAdmin {
			writeErr(w, http.StatusBadRequest, "role must be user or admin")
			return
		}
		if u.ID == p.User.ID && req.Role != store.RoleAdmin {
			writeErr(w, http.StatusBadRequest, "you cannot take your own admin role away")
			return
		}
		u.Role = req.Role
	}
	if req.Disabled != nil {
		if u.ID == p.User.ID && *req.Disabled {
			writeErr(w, http.StatusBadRequest, "you cannot disable your own account")
			return
		}
		u.Disabled = *req.Disabled
	}
	if req.Quota != nil {
		if err := validQuota(*req.Quota); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		u.Quota = *req.Quota
	}
	if req.Password != "" {
		if err := validPassword(req.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			s.mapErr(w, err)
			return
		}
		if err := s.Store.Users().SetPasswordHash(r.Context(), u.ID, hash); err != nil {
			s.mapErr(w, err)
			return
		}
	}
	updated, err := s.Store.Users().Update(r.Context(), u)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	s.Log.Info("user updated", "user", updated.Username, "role", updated.Role, "disabled", updated.Disabled, "by", p.User.Username)
	writeJSON(w, http.StatusOK, s.userWithUsage(r.Context(), updated))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.Apps.DeleteUser(r.Context(), principal(r).User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func validQuota(q store.Quota) error {
	if q.MaxApps != nil && *q.MaxApps < 0 {
		return fmt.Errorf("max_apps must be 0 (unlimited) or more")
	}
	if q.MaxStorage != nil && *q.MaxStorage != "" {
		if err := apps.ValidQuantity(*q.MaxStorage); err != nil {
			return fmt.Errorf("max_storage: %w", err)
		}
	}
	return nil
}

// ---- own account ----

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !auth.VerifyPassword(p.User.PasswordHash, body.Current) {
		writeErr(w, http.StatusForbidden, "the current password is wrong")
		return
	}
	if err := validPassword(body.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(body.New)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	if err := s.Store.Users().SetPasswordHash(r.Context(), p.User.ID, hash); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- catalog ----

func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	list, err := s.Apps.Templates(r.Context(), principal(r).User)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list, "presets": s.Apps.Presets(), "defaults": s.Apps.Defaults})
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req apps.CreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.Apps.CreateTemplate(r.Context(), principal(r).User, req)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.Apps.TemplateView(t))
}

func (s *Server) updateTemplate(w http.ResponseWriter, r *http.Request) {
	var req apps.CreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.Apps.UpdateTemplate(r.Context(), principal(r).User, r.PathValue("id"), req)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Apps.TemplateView(t))
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.Apps.DeleteTemplate(r.Context(), principal(r).User, r.PathValue("id")); err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- every app (admin) ----

func (s *Server) listAllApps(w http.ResponseWriter, r *http.Request) {
	list, err := s.Apps.ListAll(r.Context(), principal(r).User)
	if err != nil {
		s.mapErr(w, err)
		return
	}
	owners := map[string]string{}
	if users, err := s.Store.Users().List(r.Context()); err == nil {
		for _, u := range users {
			owners[u.ID] = u.Username
		}
	}
	type ownedView struct {
		apps.View
		Owner string `json:"owner"`
	}
	views := s.views(r.Context(), list)
	out := make([]ownedView, 0, len(views))
	for _, v := range views {
		out = append(out, ownedView{View: v, Owner: owners[v.OwnerID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) stopAnyApp(w http.ResponseWriter, r *http.Request) {
	a, err := s.Apps.StopAny(r.Context(), principal(r).User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Apps.View(a))
}

func (s *Server) deleteAnyApp(w http.ResponseWriter, r *http.Request) {
	a, err := s.Apps.DeleteAny(r.Context(), principal(r).User, r.PathValue("id"))
	if err != nil {
		s.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.Apps.View(a))
}

// allAppEvents streams every user's app changes (the admin overview).
func (s *Server) allAppEvents(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, apps.AllOwners)
}

// streamEvents is the SSE loop shared by the per-user and the admin feed.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, owner string) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	_ = rc.Flush()

	ch, cancel := s.Apps.Broker.Subscribe(owner)
	defer cancel()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

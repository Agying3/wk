package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"

	"workbuddy2api/internal/coordination"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/handoff"
	"workbuddy2api/internal/lifecycle"
	"workbuddy2api/internal/pool"
)

type overlapRuntime struct {
	member      *lifecycle.Membership
	store       *coordination.Store
	owner       atomic.Bool
	configPath  string
	fingerprint string
}
type overlapMembers struct {
	Version     int      `json:"version"`
	Fingerprint string   `json:"fingerprint"`
	Members     []string `json:"members"`
}

func migrateOwnerGroups(g *groups.Store, p *pool.Pool, owner bool) int {
	if !owner {
		return 0
	}
	return g.MigrateUnregistered(p.UIDs(), func(uid string) string {
		if a := p.AuthByUID(uid); a != nil {
			return a.Region()
		}
		return ""
	})
}

func overlapFingerprint(cfg *Config) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return "", err
	}
	// The relay address and static assets differ by release. Every state path,
	// credential, salt, upstream and mutable behavior must otherwise agree.
	delete(fields, "listen")
	delete(fields, "frontend_dir")
	delete(fields, "pprof")
	for key, path := range map[string]string{"shared_auth_directory": cfg.AuthDir, "shared_state_directory": filepath.Dir(cfg.StateFile)} {
		identity, err := lifecycle.DirectoryIdentity(path)
		if err != nil {
			return "", err
		}
		fields[key], _ = json.Marshal(identity)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (o *overlapRuntime) members(raw json.RawMessage) (overlapMembers, error) {
	s := overlapMembers{Version: 1}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, err
		}
		if s.Version != 1 {
			return s, errors.New("incompatible overlap members")
		}
	}
	alive := s.Members[:0]
	for _, id := range s.Members {
		ok, err := o.member.Alive(id)
		if err != nil {
			return s, err
		}
		if ok {
			alive = append(alive, id)
		}
	}
	s.Members = alive
	return s, nil
}

func (o *overlapRuntime) register(ctx context.Context, cfg *Config) error {
	fp, err := overlapFingerprint(cfg)
	if err != nil {
		return err
	}
	o.fingerprint = fp
	return o.store.Update(ctx, "deployment/members", func(raw json.RawMessage) (json.RawMessage, error) {
		s, err := o.members(raw)
		if err != nil {
			return nil, err
		}
		if len(s.Members) > 0 && s.Fingerprint != fp {
			return nil, errors.New("overlap configurations differ; reload the shared configuration before starting the candidate")
		}
		s.Fingerprint = fp
		for _, id := range s.Members {
			if id == o.member.ID {
				return json.Marshal(s)
			}
		}
		s.Members = append(s.Members, o.member.ID)
		return json.Marshal(s)
	})
}

func (o *overlapRuntime) checkConfiguration() error {
	cfg, err := Load(o.configPath)
	if err != nil {
		return err
	}
	fp, err := overlapFingerprint(cfg)
	if err != nil {
		return err
	}
	if fp != o.fingerprint {
		return errors.New("shared configuration changed after candidate initialization; restart candidate before promotion")
	}
	return nil
}

func (o *overlapRuntime) manage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handoff.IsRelayPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !o.owner.Load() {
			http.Error(w, "management belongs to the active owner", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || r.URL.Path == "/admin/unlock" {
			next.ServeHTTP(w, r)
			return
		}
		// Serializes management admission with candidate registration. A candidate
		// cannot join while an old management mutation is still running.
		served := false
		err := o.store.Update(r.Context(), "deployment/members", func(raw json.RawMessage) (json.RawMessage, error) {
			s, err := o.members(raw)
			if err != nil {
				return nil, err
			}
			if len(s.Members) > 1 {
				return nil, errors.New("management writes are paused during an overlapping upgrade")
			}
			served = true
			next.ServeHTTP(w, r)
			cfg, err := Load(o.configPath)
			if err != nil {
				return nil, err
			}
			s.Fingerprint, err = overlapFingerprint(cfg)
			if err != nil {
				return nil, err
			}
			o.fingerprint = s.Fingerprint
			return json.Marshal(s)
		})
		if err != nil && !served {
			http.Error(w, "management writes unavailable during upgrade", http.StatusServiceUnavailable)
		}
	})
}

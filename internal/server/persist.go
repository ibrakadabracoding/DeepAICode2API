package server

import (
	"net/http"

	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/logx"
)

// --- Persist state to disk ---
//
// For admin actions, mutating memory and immediately returning {"ok":true} is not enough: when the disk is full, the file is locked (a common case on Windows
// ), or the directory is read-only, the change disappears after a restart while the caller still receives success.
// Two entry points are unified here to make "save failure" observable and, for user actions, returnable to the caller.

// persist writes to disk and records the failure without changing the interface's return value.
//
// Used for cases where the result is already produced and saving is best-effort: background probe results, login-and-pool finalization,
// and real-time usage of extension endpoints. A failure leaves a warn log line but does not need to fail the current request.
func (s *Server) persist() {
	if err := s.Store.Save(); err != nil {
		logx.Warn("状态文件保存失败（改动仅存在于内存）", "err", err, "path", s.Store.Path())
	}
}

// persistOrFail writes to disk; on failure it returns 500 and false, and the caller should return immediately.
//
// Used for changes actively triggered by the user (adding/removing keys, changing settings, account governance): the semantics of such operations is
// "persisted"; silently swallowing the failure is lying to the caller.
func (s *Server) persistOrFail(w http.ResponseWriter) bool {
	if err := s.Store.Save(); err != nil {
		logx.Warn("状态文件保存失败", "err", err, "path", s.Store.Path())
		httpx.WriteErr(w, http.StatusInternalServerError,
			"状态保存失败（改动未落盘）: "+err.Error(), "server_error")
		return false
	}
	return true
}

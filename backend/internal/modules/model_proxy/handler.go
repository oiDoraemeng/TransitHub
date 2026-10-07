package model_proxy

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"transithub/backend/internal/shared/authctx"
	"transithub/backend/internal/shared/httpjson"
)

type Handler struct {
	service *Service
}

func RegisterRoutes(mux *http.ServeMux, service *Service) {
	h := &Handler{service: service}
	mux.HandleFunc("GET /api/proxy-routes", h.listRoutes)
	mux.HandleFunc("POST /api/proxy-routes", h.createRoute)
	mux.HandleFunc("PATCH /api/proxy-routes/{id}", h.updateRoute)
	mux.HandleFunc("DELETE /api/proxy-routes/{id}", h.deleteRoute)
	mux.HandleFunc("GET /api/proxy-routes/{id}/key", h.revealRouteKey)
	mux.HandleFunc("POST /api/proxy-routes/{id}/rotate-key", h.rotateRouteKey)
	mux.HandleFunc("GET /api/proxy-routes/sites", h.listSites)
	mux.HandleFunc("GET /api/proxy-routes/sites/{siteId}/groups", h.listGroups)
	mux.HandleFunc("GET /api/proxy-routes/cleanup-status", h.cleanupStatus)
	mux.HandleFunc("GET /api/model-proxies", h.listEgressProxies)
	mux.HandleFunc("POST /api/model-proxies", h.createEgressProxy)
	mux.HandleFunc("PATCH /api/model-proxies/{id}", h.updateEgressProxy)
	mux.HandleFunc("DELETE /api/model-proxies/{id}", h.deleteEgressProxy)
	mux.HandleFunc("POST /api/model-proxies/{id}/test", h.testEgressProxy)

	mux.HandleFunc("GET /api/proxy-smart-groups", h.listSmartGroups)
	mux.HandleFunc("POST /api/proxy-smart-groups", h.createSmartGroup)
	mux.HandleFunc("PATCH /api/proxy-smart-groups/{id}", h.updateSmartGroup)
	mux.HandleFunc("DELETE /api/proxy-smart-groups/{id}", h.deleteSmartGroup)
	mux.HandleFunc("POST /api/proxy-smart-groups/{id}/members", h.addMember)
	mux.HandleFunc("PATCH /api/proxy-smart-groups/{id}/members/{routeId}", h.updateMemberPolicy)
	mux.HandleFunc("DELETE /api/proxy-smart-groups/{id}/members/{routeId}", h.removeMember)
	mux.HandleFunc("GET /api/proxy-smart-groups/{id}/key", h.revealSmartGroupKey)
	mux.HandleFunc("POST /api/proxy-smart-groups/{id}/rotate-key", h.rotateSmartGroupKey)
}

func (h *Handler) listEgressProxies(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	proxies, err := h.service.ListEgressProxies(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, proxies)
}

func (h *Handler) createEgressProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input CreateEgressProxyRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	proxy, err := h.service.CreateEgressProxy(r.Context(), userID, input)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, proxy)
}

func (h *Handler) updateEgressProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input UpdateEgressProxyRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	proxy, err := h.service.UpdateEgressProxy(r.Context(), userID, r.PathValue("id"), input)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, proxy)
}

func (h *Handler) deleteEgressProxy(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.service.DeleteEgressProxy(r.Context(), userID, r.PathValue("id")); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) testEgressProxy(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	result, err := h.service.TestEgressProxy(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, result)
}

func (h *Handler) userID(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID, ok := authctx.UserID(r.Context())
	if !ok {
		httpjson.WriteError(w, http.StatusUnauthorized, "auth.errors.unauthorized")
	}
	return userID, ok
}

func (h *Handler) listRoutes(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	routes, err := h.service.ListRoutes(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, routes)
}

func (h *Handler) createRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input CreateRouteRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, 400, "invalid request body")
		return
	}
	route, key, err := h.service.CreateRoute(r.Context(), userID, input)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"route": route, "key": key})
}

func (h *Handler) updateRoute(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input UpdateRouteRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, 400, "invalid request body")
		return
	}
	route, err := h.service.UpdateRoute(r.Context(), userID, r.PathValue("id"), input)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, route)
}

func (h *Handler) deleteRoute(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.service.DeleteRoute(r.Context(), userID, r.PathValue("id")); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) revealRouteKey(w http.ResponseWriter, r *http.Request) {
	h.keyAction(w, r, OwnerRoute, false)
}
func (h *Handler) rotateRouteKey(w http.ResponseWriter, r *http.Request) {
	h.keyAction(w, r, OwnerRoute, true)
}
func (h *Handler) revealSmartGroupKey(w http.ResponseWriter, r *http.Request) {
	h.keyAction(w, r, OwnerSmartGroup, false)
}
func (h *Handler) rotateSmartGroupKey(w http.ResponseWriter, r *http.Request) {
	h.keyAction(w, r, OwnerSmartGroup, true)
}

func (h *Handler) keyAction(w http.ResponseWriter, r *http.Request, ownerType string, rotate bool) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var response KeyResponse
	var err error
	if rotate {
		response, err = h.service.RotateKey(r.Context(), userID, ownerType, r.PathValue("id"))
	} else {
		response, err = h.service.RevealKey(r.Context(), userID, ownerType, r.PathValue("id"))
	}
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, response)
}

func (h *Handler) listSites(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, h.service.ListSites(r.Context(), userID))
}

func (h *Handler) listGroups(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	groups, err := h.service.ListGroups(r.Context(), userID, r.PathValue("siteId"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, groups)
}

func (h *Handler) cleanupStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	accountID, err := h.service.currentWorkspace(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	status, err := h.service.repository.CleanupSummary(r.Context(), userID, accountID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, status)
}

func (h *Handler) listSmartGroups(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	groups, err := h.service.ListSmartGroups(r.Context(), userID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, groups)
}

func (h *Handler) createSmartGroup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input CreateSmartGroupRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, 400, "invalid request body")
		return
	}
	group, key, err := h.service.CreateSmartGroup(r.Context(), userID, input)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"group": group, "key": key})
}

func (h *Handler) updateSmartGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input UpdateSmartGroupRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, 400, "invalid request body")
		return
	}
	group, err := h.service.UpdateSmartGroup(r.Context(), userID, r.PathValue("id"), input)
	if err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, group)
}

func (h *Handler) deleteSmartGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.service.DeleteSmartGroup(r.Context(), userID, r.PathValue("id")); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input AddMemberRequest
	if err := httpjson.Decode(r, &input); err != nil || strings.TrimSpace(input.EntryKey) == "" {
		httpjson.WriteError(w, 400, "entryKey is required")
		return
	}
	if err := h.service.AddMember(r.Context(), userID, r.PathValue("id"), input.EntryKey); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]bool{"success": true})
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	if err := h.service.RemoveMember(r.Context(), userID, r.PathValue("id"), r.PathValue("routeId")); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) updateMemberPolicy(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userID(w, r)
	if !ok {
		return
	}
	var input UpdateMemberPolicyRequest
	if err := httpjson.Decode(r, &input); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.UpdateMemberPolicy(r.Context(), userID, r.PathValue("id"), r.PathValue("routeId"), input); err != nil {
		h.writeError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		httpjson.WriteError(w, reqErr.Status, reqErr.Message)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		httpjson.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if errors.Is(err, errEncryptionKeyUnavailable) {
		httpjson.WriteError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	log.Printf("[model-proxy] admin request failed: %v", err)
	httpjson.WriteError(w, http.StatusInternalServerError, "model proxy request failed")
}

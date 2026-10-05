package api

import "net/http"

func (h *Handler) storage(q *request) {
	params, err := q.query("workspace")
	if err != nil {
		q.fail(err)
		return
	}
	storage, err := h.Service.Storage(q.ctx(), q.principal, params["workspace"])
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, storage)
}

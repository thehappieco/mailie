package api

import "net/http"

func (h *Handler) storage(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	storage, err := h.Service.Storage(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, storage)
}

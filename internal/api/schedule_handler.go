package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-038/internal/service"
)

func (handler *Handler) schedulePlan(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.SchedulePlan())
}

func (handler *Handler) scheduleOrders(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.ScheduleOrders())
}

func (handler *Handler) createScheduleOrder(writer http.ResponseWriter, request *http.Request) {
	var input service.OrderRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	plan, err := handler.runtime.CreateScheduleOrder(input)
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, plan)
}

func (handler *Handler) updateScheduleOrder(writer http.ResponseWriter, request *http.Request) {
	var input service.OrderUpdateRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	plan, err := handler.runtime.UpdateScheduleOrder(chi.URLParam(request, "id"), input)
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

func (handler *Handler) startScheduleOrder(writer http.ResponseWriter, request *http.Request) {
	plan, err := handler.runtime.StartScheduleOrder(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

func (handler *Handler) completeScheduleOrder(writer http.ResponseWriter, request *http.Request) {
	plan, err := handler.runtime.CompleteScheduleOrder(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

func (handler *Handler) cancelScheduleOrder(writer http.ResponseWriter, request *http.Request) {
	plan, err := handler.runtime.CancelScheduleOrder(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

func (handler *Handler) scheduleDowntimes(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.ScheduleDowntimes())
}

func (handler *Handler) createScheduleDowntime(writer http.ResponseWriter, request *http.Request) {
	var input service.DowntimeRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	plan, err := handler.runtime.AddScheduleDowntime(input)
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, plan)
}

func (handler *Handler) deleteScheduleDowntime(writer http.ResponseWriter, request *http.Request) {
	plan, err := handler.runtime.RemoveScheduleDowntime(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, plan)
}

func writeScheduleError(writer http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "does not exist") {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	writeError(writer, http.StatusConflict, err)
}

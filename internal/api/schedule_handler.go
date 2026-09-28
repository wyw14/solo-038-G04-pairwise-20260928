package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/solo-038/internal/schedule"
	"github.com/wyw14/solo-038/internal/service"
)

type scheduleEnvelope struct {
	Order    *schedule.Order    `json:"order,omitempty"`
	Downtime *schedule.Downtime `json:"downtime,omitempty"`
	Schedule schedule.Schedule  `json:"schedule"`
}

func (handler *Handler) getSchedule(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.Schedule())
}

func (handler *Handler) listScheduledOrders(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.ScheduledOrders())
}

func (handler *Handler) getScheduledOrder(writer http.ResponseWriter, request *http.Request) {
	orders := handler.runtime.ScheduledOrders()
	for _, order := range orders {
		if order.ID == chi.URLParam(request, "id") {
			writeJSON(writer, http.StatusOK, order)
			return
		}
	}
	writeError(writer, http.StatusNotFound, schedule.ErrNotFound)
}

func (handler *Handler) createScheduledOrder(writer http.ResponseWriter, request *http.Request) {
	var input service.OrderRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	order, plan, err := handler.runtime.CreateScheduledOrder(input)
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, scheduleEnvelope{Order: &order, Schedule: plan})
}

func (handler *Handler) reviseScheduledOrder(writer http.ResponseWriter, request *http.Request) {
	var input service.OrderRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	order, plan, err := handler.runtime.ReviseScheduledOrder(chi.URLParam(request, "id"), input)
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, scheduleEnvelope{Order: &order, Schedule: plan})
}

func (handler *Handler) cancelScheduledOrder(writer http.ResponseWriter, request *http.Request) {
	plan, err := handler.runtime.CancelScheduledOrder(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, scheduleEnvelope{Schedule: plan})
}

func (handler *Handler) startScheduledOrder(writer http.ResponseWriter, request *http.Request) {
	order, plan, err := handler.runtime.StartScheduledOrder(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, scheduleEnvelope{Order: &order, Schedule: plan})
}

func (handler *Handler) completeScheduledOrder(writer http.ResponseWriter, request *http.Request) {
	order, plan, err := handler.runtime.CompleteScheduledOrder(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, scheduleEnvelope{Order: &order, Schedule: plan})
}

func (handler *Handler) listDowntimes(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.ScheduleDowntimes())
}

func (handler *Handler) createDowntime(writer http.ResponseWriter, request *http.Request) {
	var input service.DowntimeRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	window, plan, err := handler.runtime.AddScheduleDowntime(input)
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, scheduleEnvelope{Downtime: &window, Schedule: plan})
}

func (handler *Handler) removeDowntime(writer http.ResponseWriter, request *http.Request) {
	plan, err := handler.runtime.RemoveScheduleDowntime(chi.URLParam(request, "id"))
	if err != nil {
		writeScheduleError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, scheduleEnvelope{Schedule: plan})
}

func (handler *Handler) replan(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, handler.runtime.ReplanSchedule())
}

func writeScheduleError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, schedule.ErrNotFound):
		writeError(writer, http.StatusNotFound, err)
	case errors.Is(err, schedule.ErrConflict):
		writeError(writer, http.StatusConflict, err)
	default:
		writeError(writer, http.StatusBadRequest, err)
	}
}

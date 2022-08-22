package querier

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-kit/log/level"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/mux"
	"github.com/grafana/tempo/pkg/api"
	"github.com/grafana/tempo/pkg/tempopb"
	v1_trace "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/pkg/util"
	util_log "github.com/grafana/tempo/pkg/util/log"
	"github.com/grafana/tempo/tempodb"
	"github.com/opentracing/opentracing-go"
	ot_log "github.com/opentracing/opentracing-go/log"
)

const (
	BlockStartKey = "blockStart"
	BlockEndKey   = "blockEnd"
	QueryModeKey  = "mode"

	QueryModeIngesters = "ingesters"
	QueryModeBlocks    = "blocks"
	QueryModeAll       = "all"
)

// TraceByIDHandler is a http.HandlerFunc to retrieve traces
func (q *Querier) TraceByIDHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.TraceLookupQueryTimeout))
	defer cancel()

	span, ctx := opentracing.StartSpanFromContext(ctx, "Querier.TraceByIDHandler")
	defer span.Finish()

	byteID, err := api.ParseTraceID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// validate request
	blockStart, blockEnd, queryMode, timeStart, timeEnd, err := api.ValidateAndSanitizeRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	span.LogFields(
		ot_log.String("msg", "validated request"),
		ot_log.String("blockStart", blockStart),
		ot_log.String("blockEnd", blockEnd),
		ot_log.String("queryMode", queryMode),
		ot_log.String("timeStart", fmt.Sprint(timeStart)),
		ot_log.String("timeEnd", fmt.Sprint(timeEnd)))

	resp, err := q.FindTraceByID(ctx, &tempopb.TraceByIDRequest{
		TraceID:    byteID,
		BlockStart: blockStart,
		BlockEnd:   blockEnd,
		QueryMode:  queryMode,
	}, timeStart, timeEnd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// record not found here, but continue on so we can marshal metrics
	// to the body
	if resp.Trace == nil || len(resp.Trace.Batches) == 0 {
		w.WriteHeader(http.StatusNotFound)
	}

	if r.Header.Get(api.HeaderAccept) == api.HeaderAcceptProtobuf {
		span.SetTag("contentType", api.HeaderAcceptProtobuf)
		b, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set(api.HeaderContentType, api.HeaderAcceptProtobuf)
		_, err = w.Write(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		return
	}

	span.SetTag("contentType", api.HeaderAcceptJSON)
	marshaller := &jsonpb.Marshaler{}
	err = marshaller.Marshal(w, resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(api.HeaderContentType, api.HeaderAcceptJSON)
}

func (q *Querier) SearchHandler(w http.ResponseWriter, r *http.Request) {
	isSearchBlock := api.IsSearchBlock(r)

	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.Search.QueryTimeout))
	defer cancel()

	span, ctx := opentracing.StartSpanFromContext(ctx, "Querier.SearchHandler")
	defer span.Finish()

	span.SetTag("requestURI", r.RequestURI)
	span.SetTag("isSearchBlock", isSearchBlock)

	var resp *tempopb.SearchResponse
	if !isSearchBlock {
		req, err := api.ParseSearchRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		span.SetTag("SearchRequest", req.String())

		resp, err = q.SearchRecent(ctx, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		req, err := api.ParseSearchBlockRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		span.SetTag("SearchRequestBlock", req.String())

		resp, err = q.SearchBlock(ctx, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	marshaller := &jsonpb.Marshaler{}
	err := marshaller.Marshal(w, resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(api.HeaderContentType, api.HeaderAcceptJSON)
}

func (q *Querier) SearchExceptionsHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	level.Info(util_log.Logger).Log("msg", "exceptions search")
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.Search.QueryTimeout))
	defer cancel()

	span, ctx := opentracing.StartSpanFromContext(ctx, "Querier.SearchHandler")
	defer span.Finish()

	span.SetTag("requestURI", r.RequestURI)

	var searchResp *tempopb.SearchResponse
	req, err := api.ParseSearchRequest(r)
	if err != nil {
		level.Info(util_log.Logger).Log("msg", "bad request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	span.SetTag("SearchRequest", req.String())

	searchResp, err = q.SearchRecent(ctx, req)
	if err != nil {
		level.Info(util_log.Logger).Log("msg", "bad request")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	level.Info(util_log.Logger).Log("msg", "traces found", len(searchResp.Traces))
	aggregatedSpans := map[string][]*v1_trace.Span{}
	resp := tempopb.ExceptionResponse{Exceptions: []*tempopb.Exception{}}

	for _, t := range searchResp.Traces {
		byteID, _ := util.HexStringToTraceID(t.TraceID)

		level.Info(util_log.Logger).Log("msg", "trace id", t.TraceID, "from", req.Start, "to", req.End)
		resp, err := q.FindTraceByID(ctx, &tempopb.TraceByIDRequest{
			TraceID:    byteID,
			BlockStart: tempodb.BlockIDMin,
			BlockEnd:   tempodb.BlockIDMax,
			QueryMode:  "all",
		}, int64(req.Start), int64(req.End))
		if err != nil {
			level.Info(util_log.Logger).Log("msg", "bad request", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		level.Info(util_log.Logger).Log("msg", "batches found", len(resp.Trace.Batches))

		for _, b := range resp.Trace.Batches {
			for _, ils := range b.InstrumentationLibrarySpans {
				for _, s := range ils.Spans {
					for _, e := range s.Events {
						for _, a := range e.Attributes {
							if a.Key == "exception.type" {
								excId := a.Value.GetStringValue()
								aggregatedSpans[excId] = append(aggregatedSpans[excId], s)
							}
						}
					}
				}
			}
		}
	}

	for excType, spans := range aggregatedSpans {
		exc := &tempopb.Exception{
			ExceptionId: excType,
			Spans:       spans,
		}
		resp.Exceptions = append(resp.Exceptions, exc)
	}

	marshaller := &jsonpb.Marshaler{}
	err = marshaller.Marshal(w, &resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(api.HeaderContentType, api.HeaderAcceptJSON)
}

func (q *Querier) SearchTagsHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.Search.QueryTimeout))
	defer cancel()

	span, ctx := opentracing.StartSpanFromContext(ctx, "Querier.SearchTagsHandler")
	defer span.Finish()

	req := &tempopb.SearchTagsRequest{}

	resp, err := q.SearchTags(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	marshaller := &jsonpb.Marshaler{}
	err = marshaller.Marshal(w, resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(api.HeaderContentType, api.HeaderAcceptJSON)
}

func (q *Querier) SearchTagValuesHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.Search.QueryTimeout))
	defer cancel()

	span, ctx := opentracing.StartSpanFromContext(ctx, "Querier.SearchTagValuesHandler")
	defer span.Finish()

	vars := mux.Vars(r)
	tagName, ok := vars["tagName"]
	if !ok {
		http.Error(w, "please provide a tagName", http.StatusBadRequest)
		return
	}
	req := &tempopb.SearchTagValuesRequest{
		TagName: tagName,
	}

	resp, err := q.SearchTagValues(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	marshaller := &jsonpb.Marshaler{}
	err = marshaller.Marshal(w, resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(api.HeaderContentType, api.HeaderAcceptJSON)
}

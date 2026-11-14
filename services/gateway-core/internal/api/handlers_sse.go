package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// GET /v1/public/payments/{orderId}/events — streamPublicPaymentEvents
//
// 帧格式 (openapi 冻结):
//   id: <per-order monotonic uint64>\n
//   event: <SseEventType>\n
//   data: <one-line JSON>\n
//   retry: 3000\n\n
// 每 15s 输出 ": heartbeat" 注释行; Last-Event-ID 断线回放由 sse.Broker 缓冲支撑.
// ---------------------------------------------------------------------------

func (s *Server) streamPaymentEvents(w http.ResponseWriter, r *http.Request) {
	orderID, ae := parsePathUUID(r, "orderId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Orders == nil || s.deps.Streamer == nil {
		writeError(w, r, upstream("stream pipeline not wired"))
		return
	}
	order, err := s.deps.Orders.GetOrder(r.Context(), orderID)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	// Last-Event-ID 续传: header 优先; ?lastEventId= 为浏览器原生
	// EventSource (无法设置自定义 header) 的等价兜底 (FIX-D4).
	var afterSeq uint64
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	if raw != "" {
		n, perr := strconv.ParseUint(raw, 10, 64)
		if perr != nil {
			writeError(w, r, badRequest("invalid Last-Event-ID"))
			return
		}
		afterSeq = n
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, internalErr("streaming unsupported"))
		return
	}

	events, cancel, err := s.deps.Streamer.Subscribe(r.Context(), orderID, afterSeq)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeFrame := func(seq uint64, event string, data any) {
		if seq > 0 || event == "snapshot" {
			fmt.Fprintf(w, "id: %d\n", seq)
		}
		fmt.Fprintf(w, "event: %s\n", event)
		if blob, err := json.Marshal(data); err == nil {
			fmt.Fprintf(w, "data: %s\n", blob)
		}
		fmt.Fprintf(w, "retry: %d\n\n", s.deps.Config.SSERetryMillis)
		flusher.Flush()
	}

	// 连接即下发 snapshot (断线重连同样重新同步).
	if snap, ae := s.publicProjection(r, order); ae == nil {
		writeFrame(0, "snapshot", snap)
	}

	heartbeat := time.NewTicker(s.deps.Config.SSEHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			name, data := s.mapOrderEvent(r, ev.Event)
			writeFrame(ev.Seq, name, data)
			// 终态事件后服务端正向关闭 (客户端仍可按 retry 语义重连取 snapshot).
			if isTerminalOrderEvent(ev.Event) {
				return
			}
		}
	}
}

// mapOrderEvent ports.OrderEvent -> (SseEventType, data) 映射.
// ⚠ 契约缺陷: ports.OrderEvent 缺 deposit 明细字段 (tx_hash/block_number 等),
// 故经 ReadStore 反查最新入账行尽力补全 (见交付报告).
func (s *Server) mapOrderEvent(r *http.Request, ev ports.OrderEvent) (string, any) {
	now := s.deps.now()
	switch ev.Type {
	case ports.OrderEventDepositDetected:
		data := sseDepositData{}
		if s.deps.Store != nil {
			if ds, err := s.deps.Store.ListOrderDeposits(r.Context(), ev.OrderID, 1); err == nil && len(ds) > 0 {
				d := ds[0]
				data = sseDepositData{
					DepositID:       d.ID.String(),
					ChainID:         int64(d.ChainID),
					TxHash:          lowerHexHash(d.TxHash),
					LogIndex:        d.LogIndex,
					BlockNumber:     d.BlockNumber,
					FromAddress:     lowerHexAddr(d.FromAddress),
					Amount:          d.Amount,
					DepositCategory: d.Category.String(),
					Status:          d.Status.String(),
				}
			}
		}
		if o, err := s.deps.Orders.GetOrder(r.Context(), ev.OrderID); err == nil {
			data.AssetSymbol = o.AssetSymbol
		}
		return "block_detected", data

	case ports.OrderEventConfirmationUpdate:
		data := sseConfirmationData{
			CurrentConfirmations:  ev.Confirmations,
			RequiredConfirmations: ev.RequiredConfirmations,
		}
		if s.deps.Store != nil {
			if ds, err := s.deps.Store.ListOrderDeposits(r.Context(), ev.OrderID, 1); err == nil && len(ds) > 0 {
				data.DepositID = ds[0].ID.String()
				data.TxHash = lowerHexHash(ds[0].TxHash)
				data.BlockNumber = ds[0].BlockNumber
			}
			if o, err := s.deps.Orders.GetOrder(r.Context(), ev.OrderID); err == nil {
				if cur, err := s.deps.Store.GetChainSyncCursor(r.Context(), o.ChainID); err == nil {
					data.SafeHeadHeight = cur.SafeHeadHeight
				}
			}
		}
		return "confirmation_progress", data

	case ports.OrderEventExpired:
		return "expired", s.orderStatusData(r, ev, now)

	case ports.OrderEventStatusChanged:
		return sseEventNameForStatus(ev.Status), s.orderStatusData(r, ev, now)

	default:
		return sseEventNameForStatus(ev.Status), s.orderStatusData(r, ev, now)
	}
}

func (s *Server) orderStatusData(r *http.Request, ev ports.OrderEvent, now time.Time) sseOrderStatusData {
	data := sseOrderStatusData{
		OrderID:        ev.OrderID.String(),
		Status:         ev.Status.String(),
		ReceivedAmount: ev.ReceivedAmount,
		OccurredAt:     ev.OccurredAt.UTC(),
	}
	if data.OccurredAt.IsZero() {
		data.OccurredAt = now
	}
	if s.deps.Orders != nil {
		if o, err := s.deps.Orders.GetOrder(r.Context(), ev.OrderID); err == nil {
			data.ExpectedAmount = o.ExpectedAmount
			data.ExpiresAt = o.ExpiresAt.UTC()
			if o.SettledAt != nil {
				t := o.SettledAt.UTC()
				data.SettledAt = &t
			}
		}
	}
	if s.deps.Store != nil {
		if ds, err := s.deps.Store.ListOrderDeposits(r.Context(), ev.OrderID, 1); err == nil && len(ds) > 0 {
			h := lowerHexHash(ds[0].TxHash)
			data.TxHash = &h
		}
	}
	return data
}

// sseEventNameForStatus PaymentOrderStatus -> SseEventType 蛇形名.
func sseEventNameForStatus(st types.OrderStatus) string {
	return strings.ToLower(st.String())
}

// isTerminalOrderEvent 业务终态后断开流 (仅 REFUNDED/REVERSAL_RESOLVED —
// 两态均无出边). EXPIRED 不得断开: §5.2 EXPIRED -> LATE_PAID_REVIEW
// (过期后迟付入账) 是合法跃迁, 断开会使客户端错过后续事件 (FIX-D4);
// PAID/OVERPAID 因深回滚可再跃迁 — 同样保持连接.
func isTerminalOrderEvent(ev ports.OrderEvent) bool {
	if ev.Type != ports.OrderEventStatusChanged {
		return false
	}
	return ev.Status == types.OrderRefunded || ev.Status == types.OrderReversalResolved
}

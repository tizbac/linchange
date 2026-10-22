package eas

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/store"
)

// settingsOof answers the Oof part of a Settings command.
//
// Exchange ActiveSync carries automatic replies here rather than in a command
// of their own. Get reads the account's setting; Set writes it. The message is
// the same for internal and external senders because linchange keeps one text,
// and the response repeats it under the internal message so a phone that only
// reads that element still shows what will be sent.
func (e *EAS) settingsOof(c *call, w *WBXMLWriter, oof *WBXMLNode) *easErr {
	settings := store.NewOutOfOffice()
	if set := oof.FindDescendant("Set"); set != nil {
		state := asInt(set.FindDescendant("OofState"), 0)
		start := parseEASDateTime(set.TextDescendant("StartTime"))
		end := parseEASDateTime(set.TextDescendant("EndTime"))
		var startPtr, endPtr *time.Time
		if !start.IsZero() {
			startPtr = &start
		}
		if !end.IsZero() {
			endPtr = &end
		}
		message := ""
		if msg := set.FindDescendant("OofMessage"); msg != nil {
			message = msg.TextOf("ReplyMessage")
		}
		if strings.TrimSpace(message) == "" {
			message = set.TextDescendant("ReplyMessage")
		}
		current, err := settings.Get(c.ctx, e.db, c.owner())
		if err != nil {
			return &easErr{status: 6001, http: http.StatusInternalServerError,
				text: "the out-of-office setting could not be saved"}
		}
		if strings.TrimSpace(message) == "" {
			message = current.Message
		}
		if _, err := settings.Set(c.ctx, e.db, c.owner(), store.OutOfOffice{
			Enabled: state != 0,
			Start:   startPtr,
			End:     endPtr,
			Subject: current.Subject,
			Message: message,
		}); err != nil {
			return &easErr{status: 6001, http: http.StatusInternalServerError,
				text: "the out-of-office setting could not be saved"}
		}
		w.Start(nsSettings, "Oof")
		_ = w.Leaf(nsSettings, "Status", "1")
		w.End()
		return nil
	}

	setting, err := settings.Get(c.ctx, e.db, c.owner())
	if err != nil {
		return &easErr{status: 6001, http: http.StatusInternalServerError,
			text: "the out-of-office setting could not be read"}
	}
	state := 0
	if setting.Enabled {
		state = 1
		if setting.Start != nil || setting.End != nil {
			state = 2
		}
	}
	w.Start(nsSettings, "Oof")
	_ = w.Leaf(nsSettings, "Status", "1")
	_ = w.Leaf(nsSettings, "OofState", strconv.Itoa(state))
	if setting.Start != nil {
		_ = w.Leaf(nsSettings, "StartTime", easTime(*setting.Start))
	}
	if setting.End != nil {
		_ = w.Leaf(nsSettings, "EndTime", easTime(*setting.End))
	}
	w.Start(nsSettings, "OofMessage")
	w.Start(nsSettings, "AppliesToInternal")
	w.End()
	_ = w.Leaf(nsSettings, "ReplyMessage", setting.Message)
	_ = w.Leaf(nsSettings, "BodyType", "Text")
	w.End()
	w.End()
	return nil
}

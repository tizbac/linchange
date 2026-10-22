package ews

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/store"
)

// Out-of-office settings are part of EWS because that is the API Outlook uses
// for its Automatic Replies dialog. MAPI does not carry the setting itself, so
// an Outlook connected through MAPI/HTTP still changes it through this path.
//
// The response schema is the one in MS-OXWSOOF: a ResponseMessage followed by
// the OofSettings element, with the message repeated for internal and external
// senders because this server keeps one message rather than two.

type oofRequest struct {
	Mailbox struct {
		EmailAddress string `xml:"EmailAddress"`
	} `xml:"Mailbox"`
	OofSettings *oofSettingsRequest `xml:"OofSettings"`
}

type oofSettingsRequest struct {
	OofState string `xml:"OofState"`
	Duration *struct {
		StartTime string `xml:"StartTime"`
		EndTime   string `xml:"EndTime"`
	} `xml:"Duration"`
	InternalReply *struct {
		Message  string `xml:"Message"`
		BodyType string `xml:"BodyType"`
	} `xml:"InternalReply"`
	ExternalReply *struct {
		Message  string `xml:"Message"`
		BodyType string `xml:"BodyType"`
	} `xml:"ExternalReply"`
}

func xmlText(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

// oofMailbox checks that the request names the caller. An Exchange client may
// ask about its own mailbox explicitly; anything else is a request to read
// somebody else's automatic reply, which is refused.
func (c *call) oofMailbox(requested string) (string, *soapError) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	own := strings.ToLower(c.who.Address)
	if requested == "" || requested == own {
		return own, nil
	}
	return "", errorf(c.name+"Response", codeErrorAccessDenied, 200,
		"out-of-office settings belong to the signed-in mailbox")
}

func oofFault(c *call, err *soapError) string {
	return faultResponseMessage(c.name+"ResponseMessage", err)
}

func oofResponseMessage() string {
	return `<m:ResponseMessage ResponseClass="Success"><m:ResponseCode>` +
		codeNoError + `</m:ResponseCode></m:ResponseMessage>`
}

func (e *EWS) getUserOofSettings(c *call) responseCode {
	var req oofRequest
	if err := c.decode(&req); err != nil {
		return responseCode{element: "GetUserOofSettingsResponse", body: oofFault(c, err)}
	}
	address, fault := c.oofMailbox(req.Mailbox.EmailAddress)
	if fault != nil {
		return responseCode{element: "GetUserOofSettingsResponse", body: oofFault(c, fault)}
	}
	setting, err := store.NewOutOfOffice().Get(c.ctx(), e.db, address)
	if err != nil {
		return responseCode{element: "GetUserOofSettingsResponse", body: oofFault(c, e.internal("GetUserOofSettings", err))}
	}

	state := "Disabled"
	if setting.Enabled {
		state = "Enabled"
		if setting.Start != nil || setting.End != nil {
			state = "Scheduled"
		}
	}
	var duration strings.Builder
	if setting.Start != nil || setting.End != nil {
		duration.WriteString("<t:Duration>")
		if setting.Start != nil {
			duration.WriteString("<t:StartTime>" + xmlText(setting.Start.UTC().Format(time.RFC3339)) + "</t:StartTime>")
		}
		if setting.End != nil {
			duration.WriteString("<t:EndTime>" + xmlText(setting.End.UTC().Format(time.RFC3339)) + "</t:EndTime>")
		}
		duration.WriteString("</t:Duration>")
	}
	reply := func() string {
		return `<t:InternalReply><t:Message>` + xmlText(setting.Message) +
			`</t:Message><t:BodyType>Text</t:BodyType></t:InternalReply>` +
			`<t:ExternalReply><t:Message>` + xmlText(setting.Message) +
			`</t:Message><t:BodyType>Text</t:BodyType></t:ExternalReply>`
	}
	body := oofResponseMessage() +
		`<m:OofSettings><t:OofState>` + state + `</t:OofState>` +
		`<t:ExternalAudience>All</t:ExternalAudience>` +
		duration.String() + reply() + `</m:OofSettings>`
	return responseCode{element: "GetUserOofSettingsResponse", body: body}
}

func (e *EWS) setUserOofSettings(c *call) responseCode {
	var req oofRequest
	if err := c.decode(&req); err != nil {
		return responseCode{element: "SetUserOofSettingsResponse", body: oofFault(c, err)}
	}
	address, fault := c.oofMailbox(req.Mailbox.EmailAddress)
	if fault != nil {
		return responseCode{element: "SetUserOofSettingsResponse", body: oofFault(c, fault)}
	}
	if req.OofSettings == nil {
		return responseCode{element: "SetUserOofSettingsResponse",
			body: oofFault(c, errorf(c.name+"Response", codeErrorSchemaValidation, 200,
				"the request has no OofSettings"))}
	}

	current, err := store.NewOutOfOffice().Get(c.ctx(), e.db, address)
	if err != nil {
		return responseCode{element: "SetUserOofSettingsResponse", body: oofFault(c, e.internal(c.name, err))}
	}
	state := strings.ToLower(strings.TrimSpace(req.OofSettings.OofState))
	enabled := state == "enabled" || state == "scheduled"
	message := ""
	if req.OofSettings.InternalReply != nil {
		message = req.OofSettings.InternalReply.Message
	}
	if strings.TrimSpace(message) == "" && req.OofSettings.ExternalReply != nil {
		message = req.OofSettings.ExternalReply.Message
	}
	if strings.TrimSpace(message) == "" {
		message = current.Message
	}
	if strings.TrimSpace(message) == "" && enabled {
		return responseCode{element: "SetUserOofSettingsResponse",
			body: oofFault(c, errorf(c.name+"Response", codeErrorSchemaValidation, 200,
				"an enabled automatic reply needs a message"))}
	}

	var start, end *time.Time
	if req.OofSettings.Duration != nil {
		parse := func(value string) (*time.Time, error) {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, nil
			}
			t, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return nil, fmt.Errorf("bad date %q", value)
			}
			utc := t.UTC()
			return &utc, nil
		}
		if start, err = parse(req.OofSettings.Duration.StartTime); err != nil {
			return responseCode{element: "SetUserOofSettingsResponse",
				body: oofFault(c, errorf(c.name+"Response", codeErrorSchemaValidation, 200, "%v", err))}
		}
		if end, err = parse(req.OofSettings.Duration.EndTime); err != nil {
			return responseCode{element: "SetUserOofSettingsResponse",
				body: oofFault(c, errorf(c.name+"Response", codeErrorSchemaValidation, 200, "%v", err))}
		}
	}

	setting := store.OutOfOffice{
		Enabled: enabled,
		Start:   start,
		End:     end,
		Subject: current.Subject,
		Message: message,
	}
	if strings.TrimSpace(setting.Subject) == "" {
		setting.Subject = "Out of office"
	}
	if _, err := store.NewOutOfOffice().Set(c.ctx(), e.db, address, setting); err != nil {
		return responseCode{element: "SetUserOofSettingsResponse", body: oofFault(c, e.internal(c.name, err))}
	}
	return responseCode{element: "SetUserOofSettingsResponse", body: oofResponseMessage()}
}

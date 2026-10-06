package ews

import (
	"strings"
)

// Shapes.
//
// A client does not ask for "a message". It sends a shape: a base shape naming
// how much it wants, plus a list of additional fields by URI. A server that
// answered with a fixed set of properties therefore cannot be right in either
// direction -- a client that asked for one property is sent more than it asked
// for, and one that asked for a listing is sent every body on the server.
//
// The shape is parsed once and then honoured exactly, against a documented default
// for the case where the client named no shape at all. MS-OXECWS is explicit that
// the default set is the one Exchange itself includes in a listing, so that list
// is written out here rather than guessed at per operation.

// Base shapes (MS-OXECWS §2.2.2.10), lower-cased.
const (
	shapeIDOnly        = "idonly"
	shapeDefault       = "default"
	shapeAllProperties = "allproperties"
	shapeBestMatch     = "bestmatch"
)

// shape is what a client asked to be told about an object.
type shape struct {
	// base is one of the shape base values.
	base string
	// extra holds the additionally requested field URIs, normalised to lower
	// case. They are stored normalised rather than as parsed attributes because
	// every comparison is against a normalised literal, and normalising once
	// here is what keeps the three spellings of one URI -- "item:Subject",
	// PropertySet="item" Property="Subject", and FieldURI="item:Subject" -- from
	// having to be reconciled at each comparison.
	extra []string
}

// fieldURI is one requested property as it arrives (MS-OXECWS §2.2.2.11).
type fieldURI struct {
	// PropertySet is "item", "message", "calendar", "contacts" or empty.
	PropertySet string `xml:"PropertySet,attr"`
	// Property is the property name within the set.
	Property string `xml:"Property,attr"`
	// FieldURI is the short form, used by clients for properties outside the
	// named sets and by every client for older requests.
	FieldURI string `xml:"FieldURI,attr"`
	// ItemClass narrows the request to one kind of item.
	ItemClass string `xml:"ItemClass,attr"`
}

// key normalises a requested field URI.
//
// The property set is part of the identity: "item:DateTimeSent" and
// "item:DateTimeCreated" are different properties, but so are "message:IsRead"
// and "item:IsRead" as far as a client is concerned -- they arrive on the same
// element and both are legal, so the set is kept rather than discarded.
func (f fieldURI) key() string {
	part := f.Property
	if part == "" {
		part = f.FieldURI
	}
	part = strings.ToLower(strings.TrimSpace(part))
	if part == "" {
		return ""
	}
	if set := strings.ToLower(strings.TrimSpace(f.PropertySet)); set != "" {
		// A bare name in PropertySet-qualified form and the same name with no
		// set at all are the same request; normalising both to the bare name
		// would lose the distinction the protocol draws, and keeping both
		// spellings would mean answering the same property twice.
		return set + ":" + part
	}
	return part
}

// shapeRequest is a t:BaseShapeType. ItemShape and FolderShape have the same
// structure and the same meaning, so one type reads both.
type shapeRequest struct {
	Base       string       `xml:"BaseShape"`
	Additional *shapeFields `xml:"AdditionalProperties"`
}

// shapeFields is t:AdditionalProperties.
type shapeFields struct {
	Fields []fieldURI `xml:"FieldURI"`
}

// parseShape reads a shape request, defaulting to the default shape.
//
// A malformed base shape is answered as Default rather than refused: the field URI
// vocabulary is open, and refusing a request over a field this server does not
// recognise would fail a client for asking about something it legitimately wants
// to know. MS-OXECWS says the same thing from the other side -- a shape the
// server cannot interpret is answered with the default.
func parseShape(raw *shapeRequest) shape {
	out := shape{base: shapeDefault}
	if raw == nil {
		return out
	}
	switch strings.ToLower(strings.TrimSpace(raw.Base)) {
	case shapeIDOnly:
		out.base = shapeIDOnly
	case shapeAllProperties, shapeBestMatch:
		out.base = shapeAllProperties
	case shapeDefault:
		out.base = shapeDefault
	}
	if raw.Additional != nil {
		for _, f := range raw.Additional.Fields {
			if key := f.key(); key != "" {
				out.extra = append(out.extra, key)
			}
		}
	}
	return out
}

// all reports whether the client asked for everything rather than a named set.
func (s shape) all() bool { return s.base == shapeAllProperties }

// identity reports whether only identifiers were asked for.
func (s shape) identity() bool { return s.base == shapeIDOnly }

// asked reports whether the shape asks for one property.
//
// defaults is the documented default set for the kind of object the shape is
// applied to, because it is that object's default set and not the shape's: a
// folder and a message both default to something, and they are not the same
// something.
//
// A property named in the request is always included, whatever the base shape
// says. That is the exception to IdOnly that makes IdOnly usable at all: a client
// that asks for the identifiers and one extra property is asking for exactly
// that, and a server that dropped the extra property would have the client
// silently drop the value too.
func (s shape) asked(key string, defaults []string) bool {
	key = strings.ToLower(key)
	for _, e := range s.extra {
		if e == key {
			return true
		}
	}
	switch s.base {
	case shapeIDOnly:
		return false
	case shapeAllProperties:
		return true
	}
	for _, d := range defaults {
		if d == key {
			return true
		}
	}
	return false
}

// anyOf reports whether the shape asks for any of a set of keys.
//
// It is how a renderer decides between two very different renderings of the same
// object: a listing, which is cheap, and a full item, which reads the body. The
// question is not "was one of these named" but "does the client want the kind of
// answer that has any of these in it".
func (s shape) anyOf(keys ...string) bool {
	for _, k := range keys {
		if s.asked(k, nil) {
			return true
		}
	}
	return false
}

// defaultFolderFields is what a folder listing contains when no shape was given.
//
// It is the list Exchange itself sends: enough for a client to draw a folder
// pane -- name, class, counts -- and nothing that costs a body read.
var defaultFolderFields = []string{
	"folder:displayname",
	"folder:folderclass",
	"folder:totalcount",
	"folder:childfoldercount",
	"folder:unreadcount",
	"folder:extendedproperty",
	"folder:parentfolderid",
}

// defaultItemFields is what an item listing contains when no shape was given.
//
// It excludes the body deliberately. A listing of five hundred messages that
// carries five hundred bodies is a listing that times out, and every client asks
// for the bodies separately, by identifier, after it has decided which one the
// user clicked.
var defaultItemFields = []string{
	"item:subject",
	"item:itemid",
	"item:size",
	"item:hasattachments",
	"item:datetime",
	"message:from",
	"message:displayto",
	"message:displaycc",
	"message:datetimereceived",
	"message:isread",
	"message:isfolderassociated",
}

// bodyFields are the properties whose rendering reads the message blob.
//
// They are listed apart from the defaults because they are the difference between
// a listing and an item read: a request that names one of these is asking for the
// message, not for the row.
var bodyFields = []string{
	"item:body",
	"item:body",
	"item:bodytext",
	"item:bodyhtml",
	"item:bodytextplaintext",
	"item:uniquebody",
	"item:preview",
	"item:normalizedsubject",
	"item:attachment",
	"item:attachments",
	"message:deliverytime",
	"message:inreplyto",
	"message:references",
	"message:replyto",
	"message:internetmessageid",
	"message:conversationtopic",
	"message:conversationindex",
	"message:cc",
	"message:to",
	"message:bcc",
	"message:sender",
	"message:timestamp",
	"message:messagedeliverytime",
	"message:reporttime",
}

// wantsBody reports whether the shape asks for anything that needs the message
// itself rather than its row.
func (s shape) wantsBody() bool {
	if s.all() {
		return true
	}
	for _, k := range bodyFields {
		if s.asked(k, nil) {
			return true
		}
	}
	return false
}

package rpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/linchange/linchange/internal/mapihttp/props"
)

// ROP IDs (Remote Operation codes from MS-OXCROPS)
const (
	ROP_NOOP                             = 0x00
	ROP_LOGON                            = 0x01
	ROP_LOGOFF                           = 0x02
	ROP_OPEN_MESSAGE                     = 0x03
	ROP_OPEN_ATTACHMENT                  = 0x04
	ROP_CREATE_MESSAGE                   = 0x05
	ROP_GET_MESSAGE_STATUS               = 0x06
	ROP_SET_MESSAGE_STATUS               = 0x07
	ROP_SAVE_CHANGES_MESSAGE             = 0x08
	ROP_DELETE_MESSAGES                  = 0x09
	ROP_GET_MESSAGE_PATH                 = 0x0A
	ROP_OPEN_FOLDER                      = 0x0B
	ROP_CREATE_FOLDER                    = 0x0C
	ROP_DELETE_FOLDER                    = 0x0D
	ROP_GET_FOLDER_TABLE                 = 0x0E
	ROP_SET_FOLDER_PROPERTIES            = 0x0F
	ROP_GET_PROPS                        = 0x10
	ROP_SET_PROPS                        = 0x11
	ROP_GET_PROP_LIST                    = 0x12
	ROP_OPEN_AB                          = 0x13
	ROP_GET_SEARCH_CRITERIA              = 0x13
	ROP_GET_HIERARCHY_TABLE              = 0x14
	ROP_GET_CONTENTS_TABLE               = 0x15
	ROP_GET_ROWS                         = 0x16
	ROP_SEEK_ROW                         = 0x17
	ROP_SEEK_ROW_BOOKMARK                = 0x18
	ROP_GET_ROW                          = 0x19
	ROP_ABORT                            = 0x1A
	ROP_PROGRESS                         = 0x1B
	ROP_TRANSFER_DATA                    = 0x1C
	ROP_BUFFER_TOO_SMALL                 = 0x1D
	ROP_SET_COLUMNS                      = 0x1E
	ROP_SORT_TABLE                       = 0x1F
	ROP_RESTRICT                         = 0x20
	ROP_CREATE_BOOKMARK                  = 0x21
	ROP_FREE_BOOKMARK                    = 0x22
	ROP_GET_COLLAPSED_ROWS               = 0x23
	ROP_EXPAND_ROW                       = 0x24
	ROP_COLLAPSE_ROW                     = 0x25
	ROP_GET_COLLAPSE_STATE               = 0x26
	ROP_SET_COLLAPSE_STATE               = 0x27
	ROP_RESOLVE_NAMES                    = 0x28
	ROP_RESOLVE_NAMES_W                  = 0x29
	ROP_ADDRESS_TYPES                    = 0x2A
	ROP_GET_ADDRESS_TYPES                = 0x2B
	ROP_GET_DEFAULT_DIR                  = 0x2C
	ROP_GET_SPECIAL_TABLE                = 0x2D
	ROP_GET_TEMPLATE_INFO                = 0x2E
	ROP_OPEN_TEMPLATE                    = 0x2F
	ROP_GET_MATCHES                      = 0x30
	ROP_FAST_TRANSFER_SOURCE_GET_BUFFER  = 0x31
	ROP_FAST_TRANSFER_SOURCE_PUT_BUFFER  = 0x32
	ROP_FAST_TRANSFER_DEST_GET_BUFFER    = 0x33
	ROP_FAST_TRANSFER_DEST_PUT_BUFFER    = 0x34
	ROP_GET_NEW_MAIL                     = 0x35
	ROP_GET_NEW_MAIL_EX                  = 0x36
	ROP_FAST_TRANSFER_DEST_GET_BUFFER_EX = 0x37
	ROP_FAST_TRANSFER_DEST_PUT_BUFFER_EX = 0x38
	ROP_READ_STREAM                      = 0x39
	ROP_WRITE_STREAM                     = 0x3A
	ROP_COMMIT_STREAM                    = 0x3B
	ROP_GET_STREAM_SIZE                  = 0x3C
	ROP_LOCK_REGION_STREAM               = 0x3D
	ROP_UNLOCK_REGION_STREAM             = 0x3E
	ROP_COPY_TO                          = 0x3F
	ROP_COPY_PROPS                       = 0x40
	ROP_MOVE_COPY                        = 0x41
	ROP_HARD_DELETE_MESSAGES             = 0x42
	ROP_EMPTY_FOLDER                     = 0x43
	ROP_GET_RULES_TABLE                  = 0x44
	ROP_MODIFY_RULES                     = 0x45
	ROP_GET_ACL_TABLE                    = 0x46
	ROP_MODIFY_ACL                       = 0x47
)

// ROP Error codes
const (
	EC_SUCCESS          = 0x00000000
	EC_NOT_FOUND        = 0x8004010F
	EC_ACCESS_DENIED    = 0x80070005
	EC_INVALID_PARAM    = 0x80070057
	EC_NOT_ENOUGH_MEM   = 0x8007000E
	EC_BUFFER_TOO_SMALL = 0x8007007A
	EC_UNKNOWN_ERROR    = 0x80004005
)

// ROPResult represents the result of a ROP execution.
type ROPResult struct {
	ROPID  byte
	Error  uint32
	Output []byte
}

// ROPInput represents a parsed ROP request.
type ROPInput struct {
	ROPID  byte
	Opcode byte
	Input  []byte
}

// BufferSize represents the size prefix for ROP buffers (2 bytes, little-endian).
type BufferSize uint16

// ParseROPBuffer parses a ROP buffer from MS-OXCROPS format.
func ParseROPBuffer(data []byte) ([]ROPInput, error) {
	buf := bytes.NewReader(data)
	var rops []ROPInput

	for buf.Len() > 0 {
		// Read ROP ID (1 byte)
		ropID, err := buf.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		// Read ROP size (2 bytes, little-endian)
		var sizeBuf [2]byte
		if _, err := io.ReadFull(buf, sizeBuf[:]); err != nil {
			return nil, err
		}
		size := binary.LittleEndian.Uint16(sizeBuf[:])

		// Read ROP input data
		input := make([]byte, size)
		if _, err := io.ReadFull(buf, input); err != nil {
			return nil, err
		}

		rops = append(rops, ROPInput{
			ROPID:  ropID,
			Opcode: ropID,
			Input:  input,
		})
	}

	return rops, nil
}

// BuildROPBuffer serializes ROP results into MS-OXCROPS buffer format.
func BuildROPBuffer(results []ROPResult) ([]byte, error) {
	var buf bytes.Buffer

	for _, res := range results {
		// Write ROP ID
		buf.WriteByte(res.ROPID)

		// Write size (2 bytes, little-endian)
		size := uint16(len(res.Output))
		if err := binary.Write(&buf, binary.LittleEndian, size); err != nil {
			return nil, err
		}

		// Write output data
		buf.Write(res.Output)
	}

	return buf.Bytes(), nil
}

// ROPLogonInput represents the input for ROP_LOGON (MS-OXCROPS 2.2.1.1)
type ROPLogonInput struct {
	LogonFlags  uint32
	OpenFlags   uint32
	StoreState  uint32
	FolderID    [8]byte
	FolderCount uint16
	FolderIDs   [][8]byte
}

// ParseROPLogonInput parses the ROP_LOGON input buffer.
func ParseROPLogonInput(data []byte) (*ROPLogonInput, error) {
	if len(data) < 14 {
		return nil, errors.New("logon input too small")
	}
	var in ROPLogonInput
	in.LogonFlags = binary.LittleEndian.Uint32(data[0:4])
	in.OpenFlags = binary.LittleEndian.Uint32(data[4:8])
	in.StoreState = binary.LittleEndian.Uint32(data[8:12])
	copy(in.FolderID[:], data[12:20])
	in.FolderCount = binary.LittleEndian.Uint16(data[20:22])
	// Skip folder IDs for now
	return &in, nil
}

// ROPLogonOutput represents the output for ROP_LOGON (MS-OXCROPS 2.2.1.1)
type ROPLogonOutput struct {
	ErrorCode     uint32
	StoreState    uint32
	FolderCount   uint16
	FolderIDs     [][8]byte
	HasNamedProps uint8
}

// Serialize serializes the ROPLogonOutput to bytes.
func (o *ROPLogonOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	binary.Write(&buf, binary.LittleEndian, o.StoreState)
	binary.Write(&buf, binary.LittleEndian, o.FolderCount)
	for _, fid := range o.FolderIDs {
		buf.Write(fid[:])
	}
	buf.WriteByte(o.HasNamedProps)
	return buf.Bytes(), nil
}

// ROPGetPropsInput represents input for ROP_GET_PROPS
type ROPGetPropsInput struct {
	RopID            byte
	Flags            uint32
	PropertyTagCount uint16
	PropertyTags     []props.PropertyTag
}

// ParseROPGetPropsInput parses ROP_GET_PROPS input.
func ParseROPGetPropsInput(data []byte) (*ROPGetPropsInput, error) {
	if len(data) < 7 {
		return nil, errors.New("getprops input too small")
	}
	buf := bytes.NewReader(data)
	var in ROPGetPropsInput
	in.RopID, _ = buf.ReadByte()
	in.Flags = binary.LittleEndian.Uint32(readBytes(buf, 4))
	in.PropertyTagCount = binary.LittleEndian.Uint16(readBytes(buf, 2))
	in.PropertyTags = make([]props.PropertyTag, in.PropertyTagCount)
	for i := 0; i < int(in.PropertyTagCount); i++ {
		var tagBytes [4]byte
		if _, err := io.ReadFull(buf, tagBytes[:]); err != nil {
			return nil, err
		}
		in.PropertyTags[i] = props.PropertyTag(binary.LittleEndian.Uint32(tagBytes[:]))
	}
	return &in, nil
}

// ROPGetPropsOutput represents output for ROP_GET_PROPS
type ROPGetPropsOutput struct {
	ErrorCode    uint32
	RowCount     uint16
	PropertyRows []props.PropertyRow
}

// Serialize serializes ROPGetPropsOutput
func (o *ROPGetPropsOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	binary.Write(&buf, binary.LittleEndian, o.RowCount)
	for _, row := range o.PropertyRows {
		for _, pv := range row {
			if err := props.MarshalProperty(&buf, pv); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

// ROPSetPropsInput represents input for ROP_SET_PROPS
type ROPSetPropsInput struct {
	RopID byte
	Flags uint32
	Row   props.PropertyRow
}

// ParseROPSetPropsInput parses ROP_SET_PROPS input.
func ParseROPSetPropsInput(data []byte) (*ROPSetPropsInput, error) {
	if len(data) < 6 {
		return nil, errors.New("setprops input too small")
	}
	buf := bytes.NewReader(data)
	var in ROPSetPropsInput
	in.RopID, _ = buf.ReadByte()
	in.Flags = binary.LittleEndian.Uint32(readBytes(buf, 4))
	// Row follows - property values
	// In practice, we'd parse the property row
	return &in, nil
}

// ROPSetPropsOutput represents output for ROP_SET_PROPS
type ROPSetPropsOutput struct {
	ErrorCode     uint32
	PropertyCount uint16
	PropertyTags  []props.PropertyTag
}

// Serialize serializes ROPSetPropsOutput
func (o *ROPSetPropsOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	binary.Write(&buf, binary.LittleEndian, o.PropertyCount)
	for _, tag := range o.PropertyTags {
		binary.Write(&buf, binary.LittleEndian, uint32(tag))
	}
	return buf.Bytes(), nil
}

// ROPGetContentsTableInput represents input for ROP_GET_CONTENTS_TABLE
type ROPGetContentsTableInput struct {
	Flags      uint32
	TableFlags uint32
}

// ROPGetContentsTableOutput represents output for ROP_GET_CONTENTS_TABLE
type ROPGetContentsTableOutput struct {
	ErrorCode uint32
	TableID   uint32
}

// Serialize serializes the output
func (o *ROPGetContentsTableOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	binary.Write(&buf, binary.LittleEndian, o.TableID)
	return buf.Bytes(), nil
}

// ROPSetColumnsInput represents input for ROP_SET_COLUMNS
type ROPSetColumnsInput struct {
	Flags            uint32
	PropertyTagCount uint16
	PropertyTags     []props.PropertyTag
}

// ROPSetColumnsOutput represents output for ROP_SET_COLUMNS
type ROPSetColumnsOutput struct {
	ErrorCode uint32
}

// Serialize serializes the output
func (o *ROPSetColumnsOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	return buf.Bytes(), nil
}

// ROPGetRowsInput represents input for ROP_GET_ROWS
type ROPGetRowsInput struct {
	Flags    uint32
	RowCount int32
}

// ROPGetRowsOutput represents output for ROP_GET_ROWS
type ROPGetRowsOutput struct {
	ErrorCode uint32
	RowCount  uint16
	Rows      []props.PropertyRow
}

// Serialize serializes the output
func (o *ROPGetRowsOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	binary.Write(&buf, binary.LittleEndian, o.RowCount)
	for _, row := range o.Rows {
		for _, pv := range row {
			if err := props.MarshalProperty(&buf, pv); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

// ROPSeekRowInput represents input for ROP_SEEK_ROW
type ROPSeekRowInput struct {
	Flags  uint32
	Origin int32
	Offset int32
}

// ROPSeekRowOutput represents output for ROP_SEEK_ROW
type ROPSeekRowOutput struct {
	ErrorCode uint32
	RowCount  uint32
}

// Serialize serializes the output
func (o *ROPSeekRowOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	binary.Write(&buf, binary.LittleEndian, o.RowCount)
	return buf.Bytes(), nil
}

// ROPGetRowInput represents input for ROP_GET_ROW
type ROPGetRowInput struct {
	Flags uint32
}

// ROPGetRowOutput represents output for ROP_GET_ROW
type ROPGetRowOutput struct {
	ErrorCode uint32
	Row       props.PropertyRow
}

// Serialize serializes the output
func (o *ROPGetRowOutput) Serialize() ([]byte, error) {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, o.ErrorCode)
	for _, pv := range o.Row {
		if err := props.MarshalProperty(&buf, pv); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ROPGetPropsSpecificInput represents input for ROP_GET_PROPS with specific properties
type ROPGetPropsSpecificInput struct {
	Flags            uint32
	PropertyTagCount uint16
	PropertyTags     []props.PropertyTag
}

// Helper function to read bytes
func readBytes(r io.Reader, n int) []byte {
	b := make([]byte, n)
	io.ReadFull(r, b)
	return b
}

// ROP factory functions for creating common ROP outputs

// SuccessLogonOutput creates a successful logon response
func SuccessLogonOutput(storeState uint32) *ROPLogonOutput {
	return &ROPLogonOutput{
		ErrorCode:     EC_SUCCESS,
		StoreState:    storeState,
		FolderCount:   0,
		HasNamedProps: 1,
	}
}

// ErrorLogonOutput creates a failed logon response
func ErrorLogonOutput(ec uint32) *ROPLogonOutput {
	return &ROPLogonOutput{
		ErrorCode:  ec,
		StoreState: 0,
	}
}

// SuccessGetPropsOutput creates a successful getprops response
func SuccessGetPropsOutput(rows []props.PropertyRow) *ROPGetPropsOutput {
	return &ROPGetPropsOutput{
		ErrorCode:    EC_SUCCESS,
		RowCount:     uint16(len(rows)),
		PropertyRows: rows,
	}
}

// ErrorGetPropsOutput creates a failed getprops response
func ErrorGetPropsOutput(ec uint32) *ROPGetPropsOutput {
	return &ROPGetPropsOutput{
		ErrorCode: ec,
	}
}

// SuccessSetPropsOutput creates a successful setprops response
func SuccessSetPropsOutput(changedTags []props.PropertyTag) *ROPSetPropsOutput {
	return &ROPSetPropsOutput{
		ErrorCode:     EC_SUCCESS,
		PropertyCount: uint16(len(changedTags)),
		PropertyTags:  changedTags,
	}
}

// ErrorSetPropsOutput creates a failed setprops response
func ErrorSetPropsOutput(ec uint32) *ROPSetPropsOutput {
	return &ROPSetPropsOutput{
		ErrorCode: ec,
	}
}

// SuccessGetContentsTableOutput creates a successful getcontents response
func SuccessGetContentsTableOutput(tableID uint32) *ROPGetContentsTableOutput {
	return &ROPGetContentsTableOutput{
		ErrorCode: EC_SUCCESS,
		TableID:   tableID,
	}
}

// SuccessGetRowsOutput creates a successful getrows response
func SuccessGetRowsOutput(rows []props.PropertyRow) *ROPGetRowsOutput {
	return &ROPGetRowsOutput{
		ErrorCode: EC_SUCCESS,
		RowCount:  uint16(len(rows)),
		Rows:      rows,
	}
}

// ErrorGetRowsOutput creates a failed getrows response
func ErrorGetRowsOutput(ec uint32) *ROPGetRowsOutput {
	return &ROPGetRowsOutput{
		ErrorCode: ec,
	}
}

// Error output helper
func ErrorOutput(ec uint32) []byte {
	return []byte{0, 0, 0, 0, byte(ec), byte(ec >> 8), byte(ec >> 16), byte(ec >> 24)}
}

// ProcessROP is the main entry point for processing a single ROP
func ProcessROP(input ROPInput, session *MAPISession) (ROPResult, error) {
	var result ROPResult
	result.ROPID = input.ROPID

	switch input.Opcode {
	case ROP_LOGON:
		out, err := ProcessLogon(input.Input, session)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_GET_PROPS:
		out, err := ProcessGetProps(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_SET_PROPS:
		out, err := ProcessSetProps(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_GET_CONTENTS_TABLE:
		out, err := ProcessGetContentsTable(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_SET_COLUMNS:
		out, err := ProcessSetColumns(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_GET_ROWS:
		out, err := ProcessGetRows(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_SEEK_ROW:
		out, err := ProcessSeekRow(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	case ROP_GET_ROW:
		out, err := ProcessGetRow(input.Input)
		if err != nil {
			result.Error = EC_UNKNOWN_ERROR
			return result, err
		}
		result.Output, _ = out.Serialize()
	default:
		result.Error = EC_UNKNOWN_ERROR
		return result, fmt.Errorf("unsupported ROP: 0x%02X", input.Opcode)
	}

	return result, nil
}

// Placeholder session type - will be replaced with actual session
type MAPISession struct {
	ID          string
	User        string
	State       uint32
	Context     uint32
	Store       interface{} // Will be *store.Store
	Folders     map[uint32]*FolderHandle
	NextTableID uint32
}

type FolderHandle struct {
	ID       [8]byte
	Name     string
	Type     int
	ParentID [8]byte
}

// ProcessLogon processes ROP_LOGON
func ProcessLogon(input []byte, session *MAPISession) (*ROPLogonOutput, error) {
	_, err := ParseROPLogonInput(input)
	if err != nil {
		return ErrorLogonOutput(EC_INVALID_PARAM), err
	}

	// Validate logon
	// In real implementation, verify credentials, open store, etc.

	// Initialize session
	if session.Folders == nil {
		session.Folders = make(map[uint32]*FolderHandle)
	}
	session.State = 1 // Logged on

	// Return success with some default folders
	return SuccessLogonOutput(session.State), nil
}

// ProcessGetProps processes ROP_GET_PROPS
func ProcessGetProps(input []byte) (*ROPGetPropsOutput, error) {
	_, err := ParseROPGetPropsInput(input)
	if err != nil {
		return ErrorGetPropsOutput(EC_INVALID_PARAM), err
	}

	// In real implementation, fetch properties from store
	// For now, return empty success
	return SuccessGetPropsOutput(nil), nil
}

// ProcessSetProps processes ROP_SET_PROPS
func ProcessSetProps(input []byte) (*ROPSetPropsOutput, error) {
	_, err := ParseROPSetPropsInput(input)
	if err != nil {
		return ErrorSetPropsOutput(EC_INVALID_PARAM), err
	}

	// In real implementation, set properties in store
	return SuccessSetPropsOutput(nil), nil
}

// ProcessGetContentsTable processes ROP_GET_CONTENTS_TABLE
func ProcessGetContentsTable(input []byte) (*ROPGetContentsTableOutput, error) {
	// Parse input
	if len(input) < 8 {
		return &ROPGetContentsTableOutput{ErrorCode: EC_INVALID_PARAM}, nil
	}
	buf := bytes.NewReader(input)
	flags := binary.LittleEndian.Uint32(readBytes(buf, 4))
	tableFlags := binary.LittleEndian.Uint32(readBytes(buf, 4))
	_ = flags
	_ = tableFlags

	// In real implementation, create a table view
	return SuccessGetContentsTableOutput(1), nil
}

// ProcessSetColumns processes ROP_SET_COLUMNS
func ProcessSetColumns(input []byte) (*ROPSetColumnsOutput, error) {
	// Parse input
	if len(input) < 3 {
		return &ROPSetColumnsOutput{ErrorCode: EC_INVALID_PARAM}, nil
	}
	// Skip parsing for now

	return &ROPSetColumnsOutput{ErrorCode: EC_SUCCESS}, nil
}

// ProcessGetRows processes ROP_GET_ROWS
func ProcessGetRows(input []byte) (*ROPGetRowsOutput, error) {
	if len(input) < 5 {
		return &ROPGetRowsOutput{ErrorCode: EC_INVALID_PARAM}, nil
	}
	buf := bytes.NewReader(input)
	flags := binary.LittleEndian.Uint32(readBytes(buf, 4))
	rowCount := int32(binary.LittleEndian.Uint32(readBytes(buf, 4)))
	_ = flags
	_ = rowCount

	// In real implementation, fetch rows from table
	return SuccessGetRowsOutput(nil), nil
}

// ProcessSeekRow processes ROP_SEEK_ROW
func ProcessSeekRow(input []byte) (*ROPSeekRowOutput, error) {
	if len(input) < 9 {
		return &ROPSeekRowOutput{ErrorCode: EC_INVALID_PARAM}, nil
	}
	buf := bytes.NewReader(input)
	flags := binary.LittleEndian.Uint32(readBytes(buf, 4))
	origin := int32(binary.LittleEndian.Uint32(readBytes(buf, 4)))
	offset := int32(binary.LittleEndian.Uint32(readBytes(buf, 4)))
	_ = flags
	_ = origin
	_ = offset

	return &ROPSeekRowOutput{ErrorCode: EC_SUCCESS, RowCount: 0}, nil
}

// ProcessGetRow processes ROP_GET_ROW
func ProcessGetRow(input []byte) (*ROPGetRowOutput, error) {
	// Parse input
	if len(input) < 4 {
		return &ROPGetRowOutput{ErrorCode: EC_INVALID_PARAM}, nil
	}
	// flags := binary.LittleEndian.Uint32(readBytes(buf, 4))
	_ = input

	return &ROPGetRowOutput{ErrorCode: EC_SUCCESS, Row: nil}, nil
}

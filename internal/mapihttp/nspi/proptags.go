package nspi

import "sort"

// Property tag lists: the three fixed sets the address book answers with,
// the code page check that accompanies them, and the code page flavouring
// they are written in. They follow nsp_get_proptags, nsp_default_tags and
// nsp_query_columns of the reference implementation.

// flavorTag maps a tag onto the form a client asks for in a given code page,
// mirroring the reference's U() macro. The macro drops the multi-value bit
// along with the type, so PR_EMS_AB_PROXY_ADDRESSES (0x800F101F) becomes
// 0x800F001E — a single-value tag the directory has no value for, which the
// presence filter then removes. A non-Unicode GetPropList therefore does not
// advertise proxy addresses at all; that quirk is the reference's and is
// reproduced rather than corrected, since a client that wants them asks for
// the multi-value tag by name.
func flavorTag(tag uint32, unicode bool) uint32 {
	if unicode {
		return tag
	}
	return changeType(tag, ptString8)
}

// canonicalTags is the property list GetPropList reports for one record: the
// fixed set of up to 32 tags (the reference fills 16 unconditionally and 16
// more for an ordinary user), each text member flavoured for the client's
// code page, then sorted by tag and trimmed to the tags the record actually
// has a value for (MS-OXNSPI 3.1.4.1.6 point 5).
//
// props is the record's property map; tags the record lacks (a thumbnail
// photo, a title, a department — none of which linchange's directory stores)
// drop out of the list, exactly as they do for the reference for a user
// without those directory fields.
func canonicalTags(props map[uint32]any, unicode bool) []uint32 {
	tags := []uint32{
		// The fixed 16.
		flavorTag(prDisplayName, unicode),
		flavorTag(prAddrType, unicode),
		flavorTag(prEmailAddress, unicode),
		flavorTag(prEmABDisplayNamePrintable, unicode),
		prObjectType,
		prDisplayType,
		prDisplayTypeEx,
		prEntryID,
		prRecordKey,
		prOriginalEntryID,
		prSearchKey,
		prInstanceKey,
		prMappingSignature,
		prSendRichInfo,
		prTemplateID,
		prEmABObjectGUID,

		// The user 16. PR_COMMENT is in this list but has no value for
		// any record: linchange's directory, like the reference's, has
		// no comment field to fill it from.
		flavorTag(prNickname, unicode),
		flavorTag(prTitle, unicode),
		flavorTag(prPrimaryTelephoneNumber, unicode),
		flavorTag(prMobileTelephoneNumber, unicode),
		flavorTag(prHomeAddressStreet, unicode),
		flavorTag(prComment, unicode),
		flavorTag(prCompanyName, unicode),
		flavorTag(prDepartmentName, unicode),
		flavorTag(prOfficeLocation, unicode),
		flavorTag(prSmtpAddress, unicode),
		flavorTag(prAccount, unicode),
		flavorTag(prTransmitableDisplayName, unicode),
		flavorTag(prEmABProxyAddresses, unicode),
		flavorTag(prEmABHomeMDB, unicode),
		prCreationTime,
		prEmABThumbnailPhoto,
	}
	out := make([]uint32, 0, len(tags))
	for _, t := range tags {
		if _, ok := props[t]; ok {
			out = append(out, t)
		}
	}
	// The reference sorts and de-duplicates the list before trimming it,
	// so the order a client sees is ascending by tag.
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return dedupeTags(out)
}

func dedupeTags(tags []uint32) []uint32 {
	if len(tags) < 2 {
		return tags
	}
	out := tags[:1]
	for _, t := range tags[1:] {
		if out[len(out)-1] != t {
			out = append(out, t)
		}
	}
	return out
}

// defaultTags is the column set QueryRows, ResolveNames and SeekEntries fall
// back on when the client sends no column list of its own. It is a
// deliberately small set: the identification columns Outlook needs to name a
// row plus three directory fields that simply have no value here, which is
// why a response built from these defaults carries error cells.
func defaultTags() []uint32 {
	return []uint32{
		prEmABContainerID,
		prObjectType,
		prDisplayType,
		prDisplayNameA,
		prPrimaryTelephonA,
		prDepartmentNameA,
		prOfficeLocationA,
	}
}

// queryColumnTags is the column list QueryColumns advertises: the tags a
// client may put in its own column list, in the fixed order the reference
// answers with. The text members follow the Unicode flag of the request; the
// typed members never do. PR_TITLE is commented out of the reference's list
// and is left out here for the same reason.
//
// The list advertises columns this directory has no values for (a nickname,
// a telephone number, a street); they surface as error cells in any row that
// asks for them, which is what the reference serves too for users without
// those fields.
func queryColumnTags(unicode bool) []uint32 {
	utags := []uint32{
		prDisplayName,
		prNickname,
		prBusinessTelephoneNumber,
		prPrimaryTelephoneNumber,
		prMobileTelephoneNumber,
		prHomeAddressStreet,
		prComment,
		prCompanyName,
		prDepartmentName,
		prOfficeLocation,
		prAddrType,
		prSmtpAddress,
		prEmailAddress,
		prEmABDisplayNamePrintable,
		prAccount,
		prTransmitableDisplayName,
		prEmABProxyAddresses,
	}
	ntags := []uint32{
		prObjectType,
		prDisplayType,
		prDisplayTypeEx,
		prEntryID,
		prRecordKey,
		prOriginalEntryID,
		prSearchKey,
		prInstanceKey,
		prMappingSignature,
		prSendRichInfo,
		prTemplateID,
		prEmABObjectGUID,
		prCreationTime,
	}
	out := make([]uint32, 0, len(utags)+len(ntags))
	for _, t := range utags {
		out = append(out, flavorTag(t, unicode))
	}
	return append(out, ntags...)
}

// acceptableCPID reports whether a code page a client may bind or ask for
// template information with. The reference derives the set from its cpid.txt
// table with the UTF-16, UTF-32 and UTF-7 pages subtracted — those are not
// encodings MAPI strings can be carried in — so no exact list of it exists
// to copy. This is the Windows code pages such a table covers, minus those
// five explicitly: 1200 (UTF-16), 1201 (UTF-16BE), 12000 (UTF-32),
// 12001 (UTF-32BE) and 65000 (UTF-7). UTF-8 (65001) is accepted; a client
// that wants UTF-16 strings is turned away earlier, by the ecNotSupported
// that every operation reports for code page 1200.
func acceptableCPID(id uint32) bool {
	switch id {
	case 0, 437, 850, 852, 855, 857,
		860, 861, 862, 863, 864, 865, 866, 869, 874, 875,
		932, 936, 949, 950, 1026,
		1250, 1251, 1252, 1253, 1254, 1255, 1256, 1257, 1258,
		10000, 10007, 10029, 10079, 10081,
		20127, 28591, 28592, 28597, 28598, 28605,
		57002, 65001:
		return true
	}
	return false
}

// sortTypeOK reports whether STAT's sort order is one the address book can
// serve. The GAL is held in display-name order, which the reference names
// with three aliases (the plain order, its read-only form and its Unicode
// form); phonetic display-name order is accepted only as a sort type, never
// as a SeekEntries target order, because no phonetic names are stored.
func sortTypeOK(t uint32) bool {
	return t == sortTypeDisplayName ||
		t == sortTypePhoneticName ||
		t == sortTypeDisplayNameRO ||
		t == sortTypeDisplayNameW
}

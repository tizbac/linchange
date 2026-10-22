// Address books and the cards in them, as the web client sees them.

export interface ContactEmail {
  type?: string;
  value: string;
}

export interface ContactPhone {
  type?: string;
  value: string;
}

export interface ContactPostal {
  type?: string;
  street?: string;
  city?: string;
  region?: string;
  postalCode?: string;
  country?: string;
}

export interface ContactInput {
  displayName: string;
  firstName?: string;
  lastName?: string;
  emails?: ContactEmail[];
  phones?: ContactPhone[];
  organization?: string;
  department?: string;
  title?: string;
  addresses?: ContactPostal[];
  website?: string;
  notes?: string;
  birthday?: string;
}

export interface Contact extends ContactInput {
  id: number;
  bookId: number;
  uid: string;
  createdAt: string;
  updatedAt: string;
  vcard?: string;
}

export interface AddressBook {
  id: number;
  owner: string;
  workgroupId?: number;
  scope: 'personal' | 'shared' | 'organization';
  name: string;
  slug: string;
  description: string;
  level: 'read' | 'write';
  isOwner: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface Suggestion {
  address: string;
  displayName?: string;
  source: string;
  scope: string;
  bookId: number;
}

export interface VCardPreview {
  displayName: string;
  emails: string[];
  uid?: string;
  duplicate: boolean;
  match?: string;
}

export interface VCardImportResult {
  imported: number;
  skipped: number;
  failed: number;
  errors?: string[];
}

export interface AddressBookShare {
  id: number;
  bookId: number;
  subjectKind: 'user' | 'group';
  subjectAddress?: string;
  subjectGroupId?: number;
  level: 'read' | 'write';
  status: string;
  groupName?: string;
  createdAt: string;
  updatedAt: string;
}

export interface AddressBookOffer extends AddressBookShare {
  bookName: string;
  bookOwner: string;
}

export interface BookShareRequest {
  subject_kind: 'user' | 'group';
  subject_address?: string;
  subject_group_id?: number;
  level: 'read' | 'write';
}

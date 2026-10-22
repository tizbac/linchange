# Contacts and address books

Contacts live in linchange itself and are served three ways: the web **Contacts**
page, CardDAV for contact clients, and the organization directory in the
Exchange address book (NSPI) and EWS name resolution.

## Address books

Every account has a **personal** book, created on first use. It cannot be
deleted; it is where a recipient is added when you write to them.

A user can create **named** books and share them with a person or an
organization group. A share is an offer: it grants nothing until the subject
accepts, and the offer is delivered as an internal message. A group offer is
answered person by person, so one member's yes does not answer for the rest.

An administrator can create **organization** books. Every member of the
organization can read them; domain administrators can write them. There is one
default organization directory per workgroup, and creating a user adds them to
it with their display name and, if given, their telephone number.

## CardDAV

CardDAV is on the same HTTP/HTTPS listener as the web application:

- Discovery: `/.well-known/carddav` (redirects to `/carddav/`)
- Service: `/carddav/`
- Principal: `/carddav/<address>/`
- Address book home: `/carddav/<address>/contacts/`
- A book: `/carddav/<address>/contacts/<slug>/`
- A card: `/carddav/<address>/contacts/<slug>/<uid>.vcf`

A book somebody owns uses its slug. A book shared with you appears under your
home as `~book-<id>/`, so every member gets the same collection under their own
principal rather than a path into somebody else's home. The server enforces the
share's read or write level on every method, so a read-only book refuses a PUT
even if the client tries.

Authentication is HTTP Basic with the account's mail password. No extra port or
certificate is needed.

## Composer and meeting autocomplete

The composer's address book and the **Add from contacts** field on the meeting
form search every book the user can read: personal first, then shared books,
then the organization directory. Each candidate carries the name of the book it
came from, so a personal contact is visibly different from the company
directory.

The same search is used for `Check Names`-style resolution by the web client.

## Automatic updates

- **Sending** a message adds each recipient to the sender's personal book. The
  card is unnamed until something knows the person's name; an existing card is
  never overwritten.
- **Receiving** a non-junk message updates the name on an existing card that has
  none. A message from a stranger does not add them: receiving mail is not an
  instruction to put somebody in the address book, but it is the best answer
  available to "who is this?" for someone already there.

## Outlook and Exchange clients

The organization directory is exposed to Outlook through the Exchange address
book:

- **NSPI** (`/mapi/nspi`): the GAL includes the organization directory cards,
  with display name, title, company, department, business phone and mobile
  phone. Users with mailboxes are enriched from their cards; people in the
  directory without a mailbox here are added as recipients.
- **EWS `ResolveNames`**: the same directory entries resolve, with company,
  job title and phone numbers in the returned contact.

The web Contacts page and CardDAV are the ways to read and write personal and
shared books. Desktop Outlook does not speak CardDAV natively, and its own
**Contacts folder** is a MAPI store folder; mapping those folders into the MAPI
message store is a separate feature and is not implemented here. Outlook's
address book, name resolution and autocomplete do use the organization
directory above. ActiveSync (EAS) contacts are likewise not implemented.

## Ports

CardDAV and the contacts API use the existing web listener. No additional port
is required.

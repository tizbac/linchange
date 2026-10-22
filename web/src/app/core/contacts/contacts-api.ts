import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import {
  AddressBook,
  AddressBookOffer,
  AddressBookShare,
  BookShareRequest,
  Contact,
  ContactInput,
  Suggestion,
  VCardImportResult,
  VCardPreview,
} from './contacts.models';

const API = '/api/v1';

// ContactsApi is the only place in the app that names an address-book
// endpoint. CardDAV speaks the same store through a different protocol.
@Injectable({ providedIn: 'root' })
export class ContactsApi {
  private readonly http = inject(HttpClient);

  listBooks(): Promise<{ books: AddressBook[] }> {
    return this.get('/address-books/');
  }

  createBook(name: string, description: string): Promise<AddressBook> {
    return this.post('/address-books/', { name, description });
  }

  updateBook(id: number, name: string, description: string): Promise<AddressBook> {
    return this.request('patch', `/address-books/${id}`, { name, description });
  }

  deleteBook(id: number): Promise<void> {
    return this.request('delete', `/address-books/${id}`);
  }

  listContacts(bookID: number): Promise<{ contacts: Contact[] }> {
    return this.get(`/address-books/${bookID}/contacts`);
  }

  getContact(bookID: number, id: number): Promise<Contact> {
    return this.get(`/address-books/${bookID}/contacts/${id}`);
  }

  createContact(bookID: number, input: ContactInput): Promise<Contact> {
    return this.post(`/address-books/${bookID}/contacts`, input);
  }

  updateContact(bookID: number, id: number, input: ContactInput): Promise<Contact> {
    return this.request('patch', `/address-books/${bookID}/contacts/${id}`, input);
  }

  deleteContact(bookID: number, id: number): Promise<void> {
    return this.request('delete', `/address-books/${bookID}/contacts/${id}`);
  }

  // previewImport parses a vCard file without writing anything. The caller
  // uses it to show duplicate warnings when the file holds more than one card.
  previewImport(bookID: number, file: File): Promise<{ contacts: VCardPreview[]; total: number }> {
    return firstValueFrom(
      this.http.post<{ contacts: VCardPreview[]; total: number }>(
        `${API}/address-books/${bookID}/contacts/import/preview`,
        this.vcardForm(file),
      ),
    );
  }

  importContacts(bookID: number, file: File): Promise<VCardImportResult> {
    return firstValueFrom(
      this.http.post<VCardImportResult>(
        `${API}/address-books/${bookID}/contacts/import`,
        this.vcardForm(file),
      ),
    );
  }

  private vcardForm(file: File): FormData {
    const form = new FormData();
    form.append('file', file, file.name);
    return form;
  }

  // search returns autocomplete candidates from every book the caller can
  // read, each carrying the book it came from.
  search(query: string, limit = 20): Promise<{ suggestions: Suggestion[] }> {
    const params = new HttpParams().set('q', query).set('limit', String(limit));
    return this.get('/contacts/search', params);
  }

  listShares(bookID: number): Promise<{ shares: AddressBookShare[] }> {
    return this.get(`/address-books/${bookID}/shares`);
  }

  share(bookID: number, request: BookShareRequest): Promise<AddressBookShare> {
    return this.post(`/address-books/${bookID}/shares`, request);
  }

  unshare(bookID: number, shareID: number): Promise<void> {
    return this.request('delete', `/address-books/${bookID}/shares/${shareID}`);
  }

  listOffers(): Promise<{ offers: AddressBookOffer[] }> {
    return this.get('/address-books/offers');
  }

  decideOffer(shareID: number, decision: 'accept' | 'decline'): Promise<AddressBookShare> {
    return this.post(`/address-books/offers/${shareID}/${decision}`, {});
  }

  private get<T>(path: string, params?: HttpParams): Promise<T> {
    return firstValueFrom(this.http.get<T>(`${API}${path}`, { params }));
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(`${API}${path}`, body));
  }

  private request<T>(method: 'patch' | 'delete', path: string, body?: unknown): Promise<T> {
    return firstValueFrom(this.http.request<T>(method, `${API}${path}`, { body }));
  }
}

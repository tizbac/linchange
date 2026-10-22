import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ContactsApi } from '../core/contacts/contacts-api';
import {
  AddressBook,
  AddressBookOffer,
  AddressBookShare,
  Contact,
  ContactInput,
  Suggestion,
} from '../core/contacts/contacts.models';
import { errorMessage } from '../core/http-error';
import { OrganizationApi, OrganizationGroup } from '../core/organization/organization-api';

// ContactsComponent is the address book page: the books the caller can open,
// the cards in the selected one, and the offers waiting for an answer.
@Component({
  selector: 'app-contacts',
  standalone: true,
  imports: [FormsModule],
  template: `
    <main class="mx-auto max-w-6xl px-6 py-10">
      <header class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="text-2xl font-semibold tracking-tight text-slate-900">Contacts</h1>
          <p class="mt-1 text-sm text-slate-500">
            Your personal book, books shared with you, and the organization directory.
          </p>
        </div>
        <button
          type="button"
          (click)="showNewBook.set(!showNewBook())"
          class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 transition-colors"
        >
          New address book
        </button>
      </header>

      @if (error()) {
        <p class="mt-4 rounded-lg bg-rose-50 px-4 py-3 text-sm text-rose-700 ring-1 ring-rose-200">{{ error() }}</p>
      }
      @if (ok()) {
        <p class="mt-4 rounded-lg bg-emerald-50 px-4 py-3 text-sm text-emerald-700 ring-1 ring-emerald-200">{{ ok() }}</p>
      }

      @if (offers().length > 0) {
        <section class="mt-5 rounded-2xl bg-white p-5 shadow-sm ring-1 ring-slate-200">
          <h2 class="text-lg font-semibold text-slate-900">Shared with you</h2>
          @for (offer of offers(); track offer.id) {
            <div class="mt-3 flex flex-wrap items-center justify-between gap-3 rounded-lg bg-slate-50 p-3">
              <span class="text-sm text-slate-700">
                <span class="font-medium">{{ offer.bookOwner }}</span> shared
                <span class="font-medium">{{ offer.bookName }}</span> with you
                <span class="text-xs text-slate-400">({{ offer.level }})</span>
              </span>
              <span class="flex gap-2">
                <button
                  type="button"
                  (click)="decide(offer, 'accept')"
                  class="rounded-lg bg-brand-700 px-3 py-1.5 text-xs font-medium text-white hover:bg-brand-800"
                >
                  Accept
                </button>
                <button
                  type="button"
                  (click)="decide(offer, 'decline')"
                  class="rounded-lg border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-100"
                >
                  Decline
                </button>
              </span>
            </div>
          }
        </section>
      }

      @if (showNewBook()) {
        <section class="mt-5 rounded-2xl bg-white p-5 shadow-sm ring-1 ring-slate-200">
          <h2 class="text-lg font-semibold text-slate-900">New address book</h2>
          <form class="mt-3 grid gap-3 sm:grid-cols-2" (ngSubmit)="createBook()">
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Name</span>
              <input
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                name="bookName"
                [(ngModel)]="newBook.name"
                required
                placeholder="Team contacts"
              />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Description (optional)</span>
              <input
                class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                name="bookDescription"
                [(ngModel)]="newBook.description"
              />
            </label>
            <div class="flex gap-2 sm:col-span-2">
              <button type="submit" [disabled]="busy() || !newBook.name.trim()"
                class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 disabled:opacity-50">
                Create
              </button>
              <button type="button" (click)="showNewBook.set(false)"
                class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50">
                Cancel
              </button>
            </div>
          </form>
        </section>
      }

      <div class="mt-5 grid gap-5 lg:grid-cols-[18rem_minmax(0,1fr)]">
        <aside class="space-y-2">
          @for (book of books(); track book.id) {
            <button
              type="button"
              (click)="selectBook(book)"
              class="w-full rounded-xl p-3 text-left ring-1 transition-colors"
              [class]="
                selectedBook()?.id === book.id
                  ? 'bg-brand-50 ring-brand-300'
                  : 'bg-white ring-slate-200 hover:bg-slate-50'
              "
            >
              <span class="block text-sm font-medium text-slate-900">{{ book.name }}</span>
              <span class="mt-0.5 block text-xs text-slate-500">
                {{ scopeLabel(book) }} · {{ book.level }}
              </span>
            </button>
          } @empty {
            <p class="rounded-xl bg-white p-4 text-sm text-slate-500 ring-1 ring-slate-200">
              Loading address books…
            </p>
          }
        </aside>

        <section class="min-w-0">
          @if (selectedBook(); as book) {
            <div class="rounded-2xl bg-white p-5 shadow-sm ring-1 ring-slate-200">
              <div class="flex flex-wrap items-start justify-between gap-3">
                <div>
                  <h2 class="text-lg font-semibold text-slate-900">{{ book.name }}</h2>
                  <p class="text-sm text-slate-500">
                    {{ book.description || scopeLabel(book) }} · {{ book.level }}
                  </p>
                </div>
                <div class="flex flex-wrap gap-2">
                  @if (book.level === 'write') {
                    <button
                      type="button"
                      (click)="startCreate()"
                      class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800"
                    >
                      New contact
                    </button>
                  }
                  @if (book.isOwner && book.scope === 'shared') {
                    <button
                      type="button"
                      (click)="toggleShare()"
                      class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
                    >
                      Share
                    </button>
                    <button
                      type="button"
                      (click)="renameBook(book)"
                      class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
                    >
                      Rename
                    </button>
                    <button
                      type="button"
                      (click)="deleteBook(book)"
                      class="rounded-lg border border-rose-300 px-4 py-2 text-sm font-medium text-rose-700 hover:bg-rose-50"
                    >
                      Delete
                    </button>
                  }
                </div>
              </div>

              <input
                class="mt-4 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
                name="contactSearch"
                [(ngModel)]="contactQuery"
                placeholder="Search this book by name, address, phone or organization"
              />

              <ul class="mt-3 divide-y divide-slate-100">
                @for (contact of filteredContacts(); track contact.id) {
                  <li>
                    <button
                      type="button"
                      (click)="openContact(contact)"
                      class="flex w-full flex-wrap items-center justify-between gap-2 py-3 text-left hover:bg-slate-50"
                    >
                      <span class="min-w-0">
                        <span class="block text-sm font-medium text-slate-900">
                          {{ contact.displayName || contact.emails?.[0]?.value || '(no name)' }}
                        </span>
                        <span class="block text-xs text-slate-500">
                          {{ contact.organization || '' }}
                          @if (contact.organization && contact.title) { · }
                          {{ contact.title || '' }}
                        </span>
                      </span>
                      <span class="text-right text-xs text-slate-500">
                        <span class="block">{{ contact.emails?.[0]?.value || '' }}</span>
                        <span class="block">{{ contact.phones?.[0]?.value || '' }}</span>
                      </span>
                    </button>
                  </li>
                } @empty {
                  <li class="py-6 text-center text-sm text-slate-400">
                    {{ contactQuery ? 'No contact matches that.' : 'This book has no contacts yet.' }}
                  </li>
                }
              </ul>
            </div>

            @if (showShare()) {
              <div class="mt-4 rounded-2xl bg-white p-5 shadow-sm ring-1 ring-slate-200">
                <h3 class="text-base font-semibold text-slate-900">Share this address book</h3>
                <p class="mt-1 text-sm text-slate-500">
                  The person or group is offered the book by email and must accept before it appears.
                </p>
                <form class="mt-3 grid gap-3 sm:grid-cols-3" (ngSubmit)="share(book)">
                  <label class="block">
                    <span class="text-sm font-medium text-slate-700">Share with</span>
                    <select
                      class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm"
                      name="shareKind"
                      [(ngModel)]="shareForm.subjectKind"
                    >
                      <option value="user">A person</option>
                      <option value="group">A group</option>
                    </select>
                  </label>
                  @if (shareForm.subjectKind === 'user') {
                    <label class="block">
                      <span class="text-sm font-medium text-slate-700">Address</span>
                      <input
                        class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm"
                        name="shareAddress"
                        type="email"
                        [(ngModel)]="shareForm.address"
                        placeholder="bob@example.com"
                      />
                    </label>
                  } @else {
                    <label class="block">
                      <span class="text-sm font-medium text-slate-700">Group</span>
                      <select
                        class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm"
                        name="shareGroup"
                        [(ngModel)]="shareForm.groupId"
                      >
                        <option [ngValue]="0">Choose a group</option>
                        @for (group of groups(); track group.id) {
                          <option [ngValue]="group.id">{{ group.name }}</option>
                        }
                      </select>
                    </label>
                  }
                  <label class="block">
                    <span class="text-sm font-medium text-slate-700">Access</span>
                    <select
                      class="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm"
                      name="shareLevel"
                      [(ngModel)]="shareForm.level"
                    >
                      <option value="read">Read</option>
                      <option value="write">Read and write</option>
                    </select>
                  </label>
                  <div class="sm:col-span-3">
                    <button
                      type="submit"
                      [disabled]="busy()"
                      class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 disabled:opacity-50"
                    >
                      Send offer
                    </button>
                  </div>
                </form>

                @if (shares().length > 0) {
                  <ul class="mt-4 divide-y divide-slate-100">
                    @for (item of shares(); track item.id) {
                      <li class="flex flex-wrap items-center justify-between gap-2 py-2">
                        <span class="text-sm text-slate-700">
                          {{ item.subjectAddress || item.groupName || ('Group ' + item.subjectGroupId) }}
                          <span class="text-xs text-slate-400">· {{ item.level }} · {{ item.status }}</span>
                        </span>
                        <button
                          type="button"
                          (click)="unshare(book, item)"
                          class="rounded-lg border border-slate-300 px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-100"
                        >
                          Remove
                        </button>
                      </li>
                    }
                  </ul>
                }
              </div>
            }
          } @else {
            <p class="rounded-2xl bg-white p-6 text-sm text-slate-500 shadow-sm ring-1 ring-slate-200">
              Choose an address book.
            </p>
          }
        </section>
      </div>

      @if (showEditor()) {
        <section class="mt-5 rounded-2xl bg-white p-5 shadow-sm ring-1 ring-slate-200">
          <h2 class="text-lg font-semibold text-slate-900">
            {{ editingId() ? 'Edit contact' : 'New contact' }}
          </h2>
          <form class="mt-4 grid gap-4 sm:grid-cols-2" (ngSubmit)="saveContact()">
            <label class="block sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">Display name</span>
              <input class="{{ input }}" name="displayName" [(ngModel)]="draft.displayName" />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">First name</span>
              <input class="{{ input }}" name="firstName" [(ngModel)]="draft.firstName" />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Last name</span>
              <input class="{{ input }}" name="lastName" [(ngModel)]="draft.lastName" />
            </label>

            <fieldset class="sm:col-span-2">
              <legend class="text-sm font-medium text-slate-700">Email addresses</legend>
              @for (email of draft.emails ?? []; track $index) {
                <div class="mt-2 flex gap-2">
                  <select class="{{ input }} w-28" [name]="'emailType' + $index" [(ngModel)]="email.type">
                    <option value="work">Work</option>
                    <option value="home">Home</option>
                    <option value="other">Other</option>
                  </select>
                  <input
                    class="{{ input }}"
                    [name]="'emailValue' + $index"
                    [attr.data-testid]="'email-value-' + $index"
                    type="email"
                    [(ngModel)]="email.value"
                  />
                  <button type="button" (click)="removeEmail($index)" class="text-xs text-rose-600">Remove</button>
                </div>
              }
              <button type="button" (click)="addEmail()" class="mt-2 text-xs font-medium text-brand-700">
                + Add email
              </button>
            </fieldset>

            <fieldset class="sm:col-span-2">
              <legend class="text-sm font-medium text-slate-700">Phone numbers</legend>
              @for (phone of draft.phones ?? []; track $index) {
                <div class="mt-2 flex gap-2">
                  <select class="{{ input }} w-28" [name]="'phoneType' + $index" [(ngModel)]="phone.type">
                    <option value="work">Work</option>
                    <option value="cell">Mobile</option>
                    <option value="home">Home</option>
                    <option value="fax">Fax</option>
                    <option value="other">Other</option>
                  </select>
                  <input
                    class="{{ input }}"
                    [name]="'phoneValue' + $index"
                    [attr.data-testid]="'phone-value-' + $index"
                    type="tel"
                    [(ngModel)]="phone.value"
                  />
                  <button type="button" (click)="removePhone($index)" class="text-xs text-rose-600">Remove</button>
                </div>
              }
              <button type="button" (click)="addPhone()" class="mt-2 text-xs font-medium text-brand-700">
                + Add phone
              </button>
            </fieldset>

            <label class="block">
              <span class="text-sm font-medium text-slate-700">Organization</span>
              <input class="{{ input }}" name="organization" [(ngModel)]="draft.organization" />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Department</span>
              <input class="{{ input }}" name="department" [(ngModel)]="draft.department" />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Job title</span>
              <input class="{{ input }}" name="title" [(ngModel)]="draft.title" />
            </label>
            <label class="block">
              <span class="text-sm font-medium text-slate-700">Website</span>
              <input class="{{ input }}" name="website" [(ngModel)]="draft.website" />
            </label>

            <fieldset class="sm:col-span-2">
              <legend class="text-sm font-medium text-slate-700">Address</legend>
              <div class="mt-2 grid gap-2 sm:grid-cols-2">
                <input class="{{ input }}" name="street" placeholder="Street" [(ngModel)]="draft.addresses![0].street" />
                <input class="{{ input }}" name="city" placeholder="City" [(ngModel)]="draft.addresses![0].city" />
                <input class="{{ input }}" name="region" placeholder="State / region" [(ngModel)]="draft.addresses![0].region" />
                <input class="{{ input }}" name="postalCode" placeholder="Postal code" [(ngModel)]="draft.addresses![0].postalCode" />
                <input class="{{ input }}" name="country" placeholder="Country" [(ngModel)]="draft.addresses![0].country" />
              </div>
            </fieldset>

            <label class="block">
              <span class="text-sm font-medium text-slate-700">Birthday</span>
              <input class="{{ input }}" name="birthday" type="date" [(ngModel)]="draft.birthday" />
            </label>
            <label class="block sm:col-span-2">
              <span class="text-sm font-medium text-slate-700">Notes</span>
              <textarea class="{{ input }}" name="notes" rows="3" [(ngModel)]="draft.notes"></textarea>
            </label>

            <div class="flex gap-2 sm:col-span-2">
              <button type="submit" [disabled]="busy() || !draft.displayName.trim()"
                class="rounded-lg bg-brand-700 px-4 py-2 text-sm font-medium text-white hover:bg-brand-800 disabled:opacity-50">
                Save
              </button>
              @if (editingId()) {
                <button type="button" (click)="deleteCurrentContact()"
                  class="rounded-lg border border-rose-300 px-4 py-2 text-sm font-medium text-rose-700 hover:bg-rose-50">
                  Delete
                </button>
              }
              <button type="button" (click)="showEditor.set(false)"
                class="rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50">
                Cancel
              </button>
            </div>
          </form>
        </section>
      }
    </main>
  `,
})
export class ContactsComponent implements OnInit {
  private readonly api = inject(ContactsApi);
  private readonly orgApi = inject(OrganizationApi);

  protected readonly input =
    'mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500';

  protected readonly books = signal<AddressBook[]>([]);
  protected readonly offers = signal<AddressBookOffer[]>([]);
  protected readonly selectedBook = signal<AddressBook | null>(null);
  protected readonly contacts = signal<Contact[]>([]);
  protected readonly shares = signal<AddressBookShare[]>([]);
  protected readonly groups = signal<OrganizationGroup[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected readonly showNewBook = signal(false);
  protected readonly showShare = signal(false);
  protected readonly showEditor = signal(false);
  protected readonly editingId = signal<number | null>(null);
  // A plain object, not a signal: it is bound field by field through
  // ngModel, and the editor is the only place that reads it.
  protected draft: ContactInput = emptyContact();

  protected contactQuery = '';
  protected newBook = { name: '', description: '' };
  protected shareForm: {
    subjectKind: 'user' | 'group';
    address: string;
    groupId: number;
    level: 'read' | 'write';
  } = { subjectKind: 'user', address: '', groupId: 0, level: 'read' };

  async ngOnInit(): Promise<void> {
    await this.load();
    void this.orgApi.groups().then((res) => this.groups.set(res.groups ?? [])).catch(() => undefined);
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const [books, offers] = await Promise.all([this.api.listBooks(), this.api.listOffers()]);
      this.books.set(books.books ?? []);
      this.offers.set(offers.offers ?? []);
      const current = this.selectedBook();
      const next = current
        ? this.books().find((b) => b.id === current.id) ?? this.books()[0]
        : this.books()[0];
      if (next) {
        await this.selectBook(next);
      }
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async selectBook(book: AddressBook): Promise<void> {
    this.selectedBook.set(book);
    this.showEditor.set(false);
    this.showShare.set(false);
    this.contactQuery = '';
    try {
      const [contacts, shares] = await Promise.all([
        this.api.listContacts(book.id),
        book.isOwner && book.scope === 'shared' ? this.api.listShares(book.id) : Promise.resolve({ shares: [] }),
      ]);
      this.contacts.set(contacts.contacts ?? []);
      this.shares.set(shares.shares ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    }
  }

  protected filteredContacts(): Contact[] {
    const q = this.contactQuery.trim().toLowerCase();
    if (!q) return this.contacts();
    return this.contacts().filter((contact) => {
      const haystack = [
        contact.displayName,
        contact.organization,
        contact.title,
        ...(contact.emails ?? []).map((e) => e.value),
        ...(contact.phones ?? []).map((p) => p.value),
      ]
        .join(' ')
        .toLowerCase();
      return haystack.includes(q);
    });
  }

  protected scopeLabel(book: AddressBook): string {
    switch (book.scope) {
      case 'personal':
        return 'Personal';
      case 'organization':
        return 'Organization';
      default:
        return book.isOwner ? 'Shared by you' : 'Shared with you';
    }
  }

  protected startCreate(): void {
    const book = this.selectedBook();
    if (!book) return;
    this.draft = emptyContact();
    this.editingId.set(null);
    this.showEditor.set(true);
  }

  protected openContact(contact: Contact): void {
    this.draft = toDraft(contact);
    this.editingId.set(contact.id);
    this.showEditor.set(true);
  }

  protected addEmail(): void {
    this.draft.emails = [...(this.draft.emails ?? []), { type: 'work', value: '' }];
  }

  protected removeEmail(index: number): void {
    this.draft.emails = (this.draft.emails ?? []).filter((_, i) => i !== index);
  }

  protected addPhone(): void {
    this.draft.phones = [...(this.draft.phones ?? []), { type: 'work', value: '' }];
  }

  protected removePhone(index: number): void {
    this.draft.phones = (this.draft.phones ?? []).filter((_, i) => i !== index);
  }

  protected async saveContact(): Promise<void> {
    const book = this.selectedBook();
    if (!book || !this.draft.displayName.trim()) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const editing = this.editingId();
      if (editing) {
        await this.api.updateContact(book.id, editing, this.draft);
      } else {
        await this.api.createContact(book.id, this.draft);
      }
      this.ok.set(editing ? 'Contact updated.' : 'Contact added.');
      this.showEditor.set(false);
      await this.selectBook(book);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async deleteCurrentContact(): Promise<void> {
    const book = this.selectedBook();
    const id = this.editingId();
    if (!book || !id) return;
    if (!confirm('Delete this contact?')) return;
    this.busy.set(true);
    try {
      await this.api.deleteContact(book.id, id);
      this.ok.set('Contact deleted.');
      this.showEditor.set(false);
      await this.selectBook(book);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async createBook(): Promise<void> {
    if (!this.newBook.name.trim()) return;
    this.busy.set(true);
    this.error.set('');
    try {
      const book = await this.api.createBook(this.newBook.name.trim(), this.newBook.description.trim());
      this.ok.set(`${book.name} was created.`);
      this.newBook = { name: '', description: '' };
      this.showNewBook.set(false);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async renameBook(book: AddressBook): Promise<void> {
    const name = prompt('New name for the address book', book.name);
    if (!name || !name.trim() || name.trim() === book.name) return;
    this.busy.set(true);
    try {
      await this.api.updateBook(book.id, name.trim(), book.description);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async deleteBook(book: AddressBook): Promise<void> {
    if (!confirm(`Delete the address book "${book.name}" and all its contacts?`)) return;
    this.busy.set(true);
    try {
      await this.api.deleteBook(book.id);
      this.ok.set(`${book.name} was deleted.`);
      this.selectedBook.set(null);
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected toggleShare(): void {
    this.showShare.set(!this.showShare());
  }

  protected async share(book: AddressBook): Promise<void> {
    const user = this.shareForm.subjectKind === 'user';
    if (user && !this.shareForm.address.trim()) return;
    if (!user && !this.shareForm.groupId) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const share = await this.api.share(book.id, {
        subject_kind: this.shareForm.subjectKind,
        subject_address: user ? this.shareForm.address.trim() : undefined,
        subject_group_id: user ? undefined : this.shareForm.groupId,
        level: this.shareForm.level,
      });
      this.ok.set('Offer sent. It appears for them once they accept.');
      this.shareForm.address = '';
      this.shareForm.groupId = 0;
      await this.selectBook(book);
      this.showShare.set(true);
      void share;
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async unshare(book: AddressBook, share: AddressBookShare): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.unshare(book.id, share.id);
      await this.selectBook(book);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async decide(offer: AddressBookOffer, decision: 'accept' | 'decline'): Promise<void> {
    this.busy.set(true);
    try {
      await this.api.decideOffer(offer.id, decision);
      this.ok.set(decision === 'accept' ? 'Address book accepted.' : 'Offer declined.');
      await this.load();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

function emptyContact(): ContactInput {
  return {
    displayName: '',
    firstName: '',
    lastName: '',
    emails: [{ type: 'work', value: '' }],
    phones: [{ type: 'work', value: '' }],
    organization: '',
    department: '',
    title: '',
    addresses: [{ type: 'work', street: '', city: '', region: '', postalCode: '', country: '' }],
    website: '',
    notes: '',
    birthday: '',
  };
}

function toDraft(contact: Contact): ContactInput {
  return {
    displayName: contact.displayName ?? '',
    firstName: contact.firstName ?? '',
    lastName: contact.lastName ?? '',
    emails: (contact.emails ?? []).map((e) => ({ ...e })),
    phones: (contact.phones ?? []).map((p) => ({ ...p })),
    organization: contact.organization ?? '',
    department: contact.department ?? '',
    title: contact.title ?? '',
    addresses:
      contact.addresses && contact.addresses.length > 0
        ? contact.addresses.map((a) => ({ ...a }))
        : [{ type: 'work', street: '', city: '', region: '', postalCode: '', country: '' }],
    website: contact.website ?? '',
    notes: contact.notes ?? '',
    birthday: contact.birthday ?? '',
  };
}

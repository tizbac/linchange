import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import {
  Domain,
  HeaderAction,
  HeaderMatch,
  formatBytes,
} from '../core/admin/admin.models';
import { errorMessage } from '../core/http-error';
import {
  AdminButton,
  Busy,
  Field,
  Notice,
  PageEmpty,
  PageHeader,
  Panel,
  inputClass,
  tableCellClass,
  tableHeadClass,
} from '../core/admin/ui';
import { AdminNavComponent } from '../core/admin/admin-nav.component';
import { AdminPageComponent } from '../core/admin/admin-page.component';
import { AdminListFooter, AdminSearch } from '../core/admin/admin-list';

@Component({
  selector: 'app-admin-domains',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [
    FormsModule,
    AdminButton,
    Busy,
    Field,
    Notice,
    PageEmpty,
    PageHeader,
    Panel,
    AdminNavComponent,
    AdminPageComponent,
    AdminSearch,
    AdminListFooter,
  ],
  templateUrl: './admin-domains.component.html',
})
export class AdminDomainsComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;
  protected readonly bytes = formatBytes;

  protected readonly domains = signal<Domain[]>([]);
  protected readonly total = signal(0);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;

  protected draft = {
    name: '',
    description: '',
    quotaMiB: null as number | null,
    isLocal: true,
    antispamNetworks: '',
  };

  // Editing state
  protected editing = signal<string | null>(null);
  protected editDraft = {
    name: '',
    description: '',
    quotaMiB: null as number | null,
    isLocal: true,
    dailySendLimit: null as number | null,
    dkimSelector: '',
    // Comma- or space-separated addresses and networks, as an operator types
    // them. The server parses and stores them in canonical form.
    antispamNetworks: '',
    rules: [] as RuleDraft[],
  };
  // Rules removed from the draft while editing. They are only deleted when the
  // form is saved, so cancelling leaves them in place.
  protected removedRules: number[] = [];

  protected readonly matches: HeaderMatch[] = ['contains', 'is', 'starts_with', 'ends_with', 'exists'];
  protected readonly actions: HeaderAction[] = ['junk', 'reject', 'discard'];

  ngOnInit(): void {
    this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listDomains({
        search: this.query || undefined,
        limit: this.pageSize,
        offset: (this.page - 1) * this.pageSize,
      });
      this.domains.set(res.domains ?? []);
      this.total.set(res.total);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected search(value: string): void {
    this.query = value;
    this.page = 1;
    void this.reload();
  }

  protected goPage(page: number): void {
    this.page = page;
    void this.reload();
  }

  protected toggleCreate(): void {
    this.creating.update((v) => !v);
    this.ok.set('');
  }

  protected async create(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      // The quota is entered in MiB because that is what an operator reasons
      // about, and the server stores bytes. Sending a raw byte count from a
      // number input is a reliable way to set a mailbox to 16 EiB.
      const quota =
        this.draft.quotaMiB === null || this.draft.quotaMiB === undefined
          ? undefined
          : Math.round(this.draft.quotaMiB * 1024 * 1024);
      await this.api.createDomain({
        name: this.draft.name.trim(),
        is_local: this.draft.isLocal,
        description: this.draft.description.trim(),
        antispam_networks: parseNetworks(this.draft.antispamNetworks),
        ...(quota === undefined ? {} : { default_quota_bytes: quota }),
      });
      this.ok.set(`${this.draft.name.trim()} was created.`);
      this.draft = {
        name: '',
        description: '',
        quotaMiB: null,
        isLocal: true,
        antispamNetworks: '',
      };
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  // toggleStatus moves a domain one step rather than offering a free-text
  // status, because the only three values are the three legal ones and a
  // dropdown that can hold an invalid value is a way to get a 400.
  protected async toggleStatus(d: Domain): Promise<void> {
    const next: Domain['status'] = d.status === 'active' ? 'suspended' : 'active';
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.setDomainStatus(d.name, next);
      this.ok.set(`${d.name} is now ${next}.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected nextStatusLabel(d: Domain): string {
    return d.status === 'active' ? 'Suspend' : 'Activate';
  }

  protected statusClass(status: Domain['status']): string {
    switch (status) {
      case 'active':
        return 'bg-emerald-50 text-emerald-700 ring-emerald-200';
      case 'suspended':
        return 'bg-amber-50 text-amber-800 ring-amber-200';
      default:
        return 'bg-slate-100 text-slate-600 ring-slate-200';
    }
  }

  protected async openEdit(d: Domain): Promise<void> {
    this.editDraft = {
      name: d.name,
      description: d.description ?? '',
      quotaMiB: d.default_quota_bytes ? Math.round(d.default_quota_bytes / 1024 / 1024) : null,
      isLocal: d.is_local,
      dailySendLimit: d.daily_send_limit ?? null,
      dkimSelector: d.dkim_selector ?? '',
      antispamNetworks: (d.antispam_networks ?? []).join(', '),
      rules: [],
    };
    this.removedRules = [];
    this.error.set('');
    this.ok.set('');

    // The header rules are a separate collection, so the edit panel has to read
    // them before it can show them. Reading them first also matters for change
    // detection: the panel is OnPush, and opening it renders once, so rules
    // loaded afterwards would never appear. A failure here is reported rather
    // than silently showing an empty list, which would look like the domain has
    // no rules when the server is simply unreachable.
    try {
      const res = await this.api.listDomainHeaderRules(d.name);
      this.editDraft.rules = (res.rules ?? []).map((rule) => ({
        id: rule.id,
        header: rule.header,
        value: rule.value,
        match: rule.match,
        action: rule.action,
        caseSensitive: rule.case_sensitive,
        enabled: rule.enabled,
      }));
    } catch (err) {
      this.error.set(errorMessage(err));
    }

    this.editing.set(d.name);
  }

  protected addRule(): void {
    this.editDraft.rules.push({
      id: 0,
      header: '',
      value: '',
      match: 'contains',
      action: 'junk',
      caseSensitive: false,
      enabled: true,
    });
  }

  protected removeRule(index: number): void {
    const rule = this.editDraft.rules[index];
    if (rule && rule.id > 0) {
      this.removedRules.push(rule.id);
    }
    this.editDraft.rules.splice(index, 1);
  }

  protected cancelEdit(): void {
    this.editing.set(null);
    this.removedRules = [];
    this.error.set('');
    this.ok.set('');
  }

  protected async saveEdit(): Promise<void> {
    const name = this.editing();
    if (!name) return;

    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const quota = this.editDraft.quotaMiB === null || this.editDraft.quotaMiB === undefined
        ? undefined
        : Math.round(this.editDraft.quotaMiB * 1024 * 1024);

      await this.api.updateDomain(name, {
        description: this.editDraft.description.trim(),
        is_local: this.editDraft.isLocal,
        default_quota_bytes: quota,
        daily_send_limit: this.editDraft.dailySendLimit ?? undefined,
        dkim_selector: this.editDraft.dkimSelector.trim() || undefined,
        antispam_networks: parseNetworks(this.editDraft.antispamNetworks),
      });

      // The rules are saved one by one, in the order they appear in the panel,
      // because the delivery path reads them in that order. A row the operator
      // added and left blank is a rule that was never meant to be stored.
      for (const rule of this.editDraft.rules) {
        const header = rule.header.trim();
        if (!header) continue;
        const body = {
          header,
          value: rule.value.trim(),
          match: rule.match,
          action: rule.action,
          case_sensitive: rule.caseSensitive,
          enabled: rule.enabled,
        };
        if (rule.id > 0) {
          await this.api.updateDomainHeaderRule(name, rule.id, body);
        } else {
          await this.api.createDomainHeaderRule(name, body);
        }
      }
      for (const id of this.removedRules) {
        await this.api.deleteDomainHeaderRule(name, id);
      }
      this.removedRules = [];

      this.ok.set(`${name} was updated.`);
      this.editing.set(null);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

// RuleDraft is the editing shape of a header rule. A rule with id 0 has not
// been stored yet; it is a row the operator is writing.
interface RuleDraft {
  id: number;
  header: string;
  value: string;
  match: HeaderMatch;
  action: HeaderAction;
  caseSensitive: boolean;
  enabled: boolean;
}

// parseNetworks turns the free-text list an operator types into the array the
// server stores. Separators are commas, spaces and newlines, because nobody
// types a JSON array into a settings field.
function parseNetworks(raw: string): string[] {
  return raw.split(/[\s,]+/).filter((entry) => entry.length > 0);
}

import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { ThemeRow } from '../core/admin/admin.models';
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

// Themes are uploaded as ZIPs and stored server-side, so this page is the only
// way a stylesheet can enter the system. The preview link opens theme.css in a
// new tab; selecting a theme happens in the top bar or on an organization.
@Component({
  selector: 'app-admin-themes',
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
  template: `
    <admin-page>
      <admin-nav />
      <div class="space-y-6">
        <admin-page-header
          title="Themes"
          hint="Upload a ZIP containing theme.css at its root or in one top-level folder,
            together with any images the stylesheet references. A custom theme can be chosen
            by users from the top bar and set as an organization's default."
        >
          <admin-button (pressed)="reload()">Refresh</admin-button>
        </admin-page-header>

        <admin-notice [message]="error()" tone="error" />
        <admin-notice [message]="ok()" tone="ok" />

        <admin-panel title="Upload a theme">
          <form class="grid gap-4 sm:grid-cols-2" (ngSubmit)="upload()">
            <admin-field label="Name" hint="Shown in the theme selector.">
              <input class="{{ inputClass }}" name="name" [(ngModel)]="name" required />
            </admin-field>
            <admin-field
              label="ZIP file"
              hint="Must contain theme.css; images and fonts may sit beside it and be referenced relatively."
            >
              <input type="file" accept=".zip,application/zip" (change)="choose($event)" />
            </admin-field>
            <div class="sm:col-span-2 flex items-center gap-3">
              <admin-button variant="primary" type="submit" [disabled]="busy() || !file">
                Upload
              </admin-button>
              <admin-busy [busy]="busy()" label="Uploading…" />
            </div>
          </form>
        </admin-panel>

        <admin-panel title="Built-in themes" [bodyClass]="'p-0'">
          <table class="w-full">
            <thead class="border-b border-slate-200 bg-slate-50">
              <tr>
                <th class="{{ head }}">Name</th>
                <th class="{{ head }}">Mode</th>
                <th class="{{ head }}"></th>
              </tr>
            </thead>
            <tbody>
              @for (b of builtins; track b.name) {
                <tr class="border-b border-slate-100">
                  <td class="{{ cell }} font-medium">{{ b.name }}</td>
                  <td class="{{ cell }} text-slate-500">Read-only</td>
                  <td class="{{ cell }} text-right">
                    <admin-button [disabled]="busy()" (pressed)="download(b.name)">
                      Export package
                    </admin-button>
                  </td>
                </tr>
              }
            </tbody>
          </table>
        </admin-panel>

        @if (loading()) {
          <admin-panel><admin-busy [busy]="true" label="Loading themes…" /></admin-panel>
        } @else if (themes().length === 0) {
          <admin-panel [bodyClass]="'p-0'">
            <admin-empty text="No custom themes yet. Upload a ZIP to add one." />
          </admin-panel>
        } @else {
          <admin-panel [bodyClass]="'p-0'">
            <div class="px-4 pt-4">
              <admin-search [query]="query" (search)="search($event)" placeholder="Search themes…" />
            </div>
            <table class="w-full">
              <thead class="border-b border-slate-200 bg-slate-50">
                <tr>
                  <th class="{{ head }}">Name</th>
                  <th class="{{ head }}">Uploaded by</th>
                  <th class="{{ head }}">Stylesheet</th>
                  <th class="{{ head }}"></th>
                </tr>
              </thead>
              <tbody>
                @for (t of themes(); track t.uid) {
                  <tr class="border-b border-slate-100">
                    <td class="{{ cell }} font-medium">{{ t.name }}</td>
                    <td class="{{ cell }}">{{ t.created_by }}</td>
                    <td class="{{ cell }}">
                      <a [href]="t.css" target="_blank" rel="noopener" class="text-brand-700 hover:underline">
                        theme.css
                      </a>
                    </td>
                    <td class="{{ cell }} text-right">
                      <admin-button variant="danger" [disabled]="busy()" (pressed)="remove(t)">
                        Delete
                      </admin-button>
                    </td>
                  </tr>
                }
              </tbody>
            </table>
            <admin-list-footer [page]="page" [total]="total()" [pageSize]="pageSize" (pageChange)="goPage($event)" />
          </admin-panel>
        }
      </div>
    </admin-page>
  `,
})
export class AdminThemesComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly themes = signal<ThemeRow[]>([]);
  protected readonly total = signal(0);
  protected query = '';
  protected page = 1;
  protected readonly pageSize = 20;
  // The built-in themes are not rows and cannot be edited or deleted. Their
  // export is a starter ZIP, which is the supported way to make a custom theme
  // that begins from one of them.
  protected readonly builtins = [
    { name: 'Light' },
    { name: 'Dark' },
    { name: 'System' },
  ];
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  protected name = '';
  protected file: File | null = null;

  ngOnInit(): void {
    void this.reload();
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listThemes({
        search: this.query || undefined,
        limit: this.pageSize,
        offset: (this.page - 1) * this.pageSize,
      });
      this.themes.set(res.themes ?? []);
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

  protected choose(event: Event): void {
    this.file = (event.target as HTMLInputElement).files?.[0] ?? null;
  }

  protected async upload(): Promise<void> {
    if (!this.file || this.busy()) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const theme = await this.api.uploadTheme(this.name.trim() || this.file.name, this.file);
      this.ok.set(`${theme.name} was uploaded.`);
      this.name = '';
      this.file = null;
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async download(name: string): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      const blob = await this.api.exportThemeExample(name);
      const url = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = url;
      link.download = `linchange-theme-${name.toLowerCase()}.zip`;
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
      this.ok.set(`${name} theme package downloaded.`);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(theme: ThemeRow): Promise<void> {
    if (!confirm(`Delete the theme ${theme.name}?`)) return;
    this.busy.set(true);
    this.error.set('');
    this.ok.set('');
    try {
      await this.api.deleteTheme(theme.uid);
      this.ok.set(`${theme.name} was deleted.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}

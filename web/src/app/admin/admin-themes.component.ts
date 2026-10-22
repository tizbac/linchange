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

        @if (loading()) {
          <admin-panel><admin-busy [busy]="true" label="Loading themes…" /></admin-panel>
        } @else if (themes().length === 0) {
          <admin-panel [bodyClass]="'p-0'">
            <admin-empty text="No custom themes yet. Upload a ZIP to add one." />
          </admin-panel>
        } @else {
          <admin-panel [bodyClass]="'p-0'">
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
      const res = await this.api.listThemes();
      this.themes.set(res.themes ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
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

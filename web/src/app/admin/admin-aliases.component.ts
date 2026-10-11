import { ChangeDetectionStrategy, Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApi } from '../core/admin/admin-api';
import { Alias, AliasTarget } from '../core/admin/admin.models';
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

interface TargetDraft {
  targetKind: 'user' | 'shared' | 'workgroup';
  targetAddress: string;
  workgroupId: number | null;
  position: number;
}

@Component({
  selector: 'app-admin-aliases',
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
  templateUrl: './admin-aliases.component.html',
})
export class AdminAliasesComponent implements OnInit {
  private readonly api = inject(AdminApi);

  protected readonly head = tableHeadClass;
  protected readonly cell = tableCellClass;
  protected readonly inputClass = inputClass;

  protected readonly aliases = signal<Alias[]>([]);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly creating = signal(false);
  protected readonly error = signal('');
  protected readonly ok = signal('');

  // For creating new alias
  protected draft = {
    address: '',
    displayName: '',
    description: '',
  };
  protected targets = signal<TargetDraft[]>([{ targetKind: 'user', targetAddress: '', workgroupId: null, position: 0 }]);

  // For editing targets
  protected editingTargets = signal<Alias | null>(null);
  protected editTargetsList = signal<TargetDraft[]>([]);

  ngOnInit(): void {
    this.reload();
  }

  protected targetKindClass(kind: string): string {
    switch (kind) {
      case 'user': return 'bg-blue-50 text-blue-700 ring-blue-200';
      case 'shared': return 'bg-violet-50 text-violet-700 ring-violet-200';
      case 'workgroup': return 'bg-amber-50 text-amber-700 ring-amber-200';
      default: return 'bg-slate-50 text-slate-700 ring-slate-200';
    }
  }

  protected onKindChange(t: TargetDraft): void {
    if (t.targetKind === 'workgroup') {
      t.targetAddress = '';
    } else {
      t.workgroupId = null;
    }
  }

  protected addTarget(): void {
    const nextPos = this.targets().length > 0
      ? Math.max(...this.targets().map(t => t.position)) + 1
      : 0;
    this.targets.update(arr => [...arr, { targetKind: 'user', targetAddress: '', workgroupId: null, position: nextPos }]);
  }

  protected removeTarget(i: number): void {
    this.targets.update(arr => arr.filter((_, idx) => idx !== i));
  }

  protected addEditTarget(): void {
    const nextPos = this.editTargetsList().length > 0
      ? Math.max(...this.editTargetsList().map(t => t.position)) + 1
      : 0;
    this.editTargetsList.update(arr => [...arr, { targetKind: 'user', targetAddress: '', workgroupId: null, position: nextPos }]);
  }

  protected removeEditTarget(i: number): void {
    this.editTargetsList.update(arr => arr.filter((_, idx) => idx !== i));
  }

  protected onEditKindChange(t: TargetDraft): void {
    if (t.targetKind === 'workgroup') {
      t.targetAddress = '';
    } else {
      t.workgroupId = null;
    }
  }

  protected editTargets(a: Alias): void {
    this.editingTargets.set(a);
    this.editTargetsList.set(a.targets.map(t => ({
      targetKind: t.target_kind as 'user' | 'shared' | 'workgroup',
      targetAddress: t.target_address ?? '',
      workgroupId: t.workgroup_id,
      position: t.position,
    })));
  }

  protected cancelEditTargets(): void {
    this.editingTargets.set(null);
    this.editTargetsList.set([]);
  }

  protected async saveTargets(): Promise<void> {
    const a = this.editingTargets();
    if (!a) return;

    this.busy.set(true);
    this.error.set('');
    try {
      const targets = this.editTargetsList().map(t => ({
        target_kind: t.targetKind,
        target_address: t.targetKind === 'workgroup' ? undefined : t.targetAddress,
        workgroup_id: t.targetKind === 'workgroup' ? (t.workgroupId ?? undefined) : undefined,
        position: t.position,
      }));
      await this.api.updateAliasTargets(a.address, targets);
      this.ok.set(`Targets for ${a.address} updated.`);
      this.cancelEditTargets();
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async reload(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const res = await this.api.listAliases();
      this.aliases.set(res.aliases ?? []);
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      const targets = this.targets().map(t => ({
        target_kind: t.targetKind,
        target_address: t.targetKind === 'workgroup' ? undefined : t.targetAddress,
        workgroup_id: t.targetKind === 'workgroup' ? (t.workgroupId ?? undefined) : undefined,
        position: t.position,
      }));
      await this.api.createAlias({
        address: this.draft.address.trim(),
        targets,
        display_name: this.draft.displayName.trim(),
        description: this.draft.description.trim(),
      });
      this.ok.set(`${this.draft.address.trim()} created with ${targets.length} target(s).`);
      this.draft = { address: '', displayName: '', description: '' };
      this.targets.set([{ targetKind: 'user', targetAddress: '', workgroupId: null, position: 0 }]);
      this.creating.set(false);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async toggleEnabled(a: Alias): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.setAliasEnabled(a.address, !a.enabled);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }

  protected async remove(a: Alias): Promise<void> {
    if (!confirm(`Delete the alias ${a.address}? Mail sent to it will bounce.`)) return;
    this.busy.set(true);
    this.error.set('');
    try {
      await this.api.deleteAlias(a.address);
      this.ok.set(`${a.address} was deleted.`);
      await this.reload();
    } catch (err) {
      this.error.set(errorMessage(err));
    } finally {
      this.busy.set(false);
    }
  }
}
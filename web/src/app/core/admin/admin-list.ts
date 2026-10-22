import { Component, computed, input, output } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminButton } from './ui';

// AdminSearch and AdminListFooter are the two ends of a server-paged table.
// They deliberately own no data: the page in the component is the page the
// server returned, and neither the browser nor the component slices a larger
// list.
@Component({
  selector: 'admin-search',
  standalone: true,
  imports: [FormsModule],
  template: `
    <label class="block w-full max-w-sm">
      <span class="sr-only">Search</span>
      <input
        type="search"
        [value]="query()"
        (input)="onInput($any($event.target).value)"
        (keyup.enter)="submit($any($event.target).value)"
        [placeholder]="placeholder()"
        class="w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm text-slate-900 placeholder:text-slate-400 focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
      />
    </label>
  `,
})
export class AdminSearch {
  readonly query = input('');
  readonly placeholder = input('Search all fields…');
  readonly search = output<string>();
  private timer?: ReturnType<typeof setTimeout>;

  protected onInput(value: string): void {
    if (this.timer) clearTimeout(this.timer);
    // A short pause keeps a typed address from becoming one request per key.
    this.timer = setTimeout(() => this.search.emit(value), 250);
  }

  protected submit(value: string): void {
    if (this.timer) clearTimeout(this.timer);
    this.search.emit(value);
  }
}

@Component({
  selector: 'admin-list-footer',
  standalone: true,
  imports: [AdminButton],
  template: `
    @if (total() > 0) {
      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-slate-200 px-4 py-3 text-sm text-slate-500">
        <span>{{ first() }}–{{ last() }} of {{ total() }}</span>
        <div class="flex items-center gap-2">
          <admin-button (pressed)="previous()" [disabled]="page() <= 1">
            Previous
          </admin-button>
          <span class="text-xs text-slate-400">Page {{ page() }} of {{ pages() }}</span>
          <admin-button (pressed)="next()" [disabled]="page() >= pages()">
            Next
          </admin-button>
        </div>
      </div>
    }
  `,
})
export class AdminListFooter {
  readonly page = input(1);
  readonly total = input(0);
  readonly pageSize = input(20);
  readonly pageChange = output<number>();
  readonly pages = computed(() => Math.max(1, Math.ceil(this.total() / this.pageSize())));

  protected first(): number {
    return (this.page() - 1) * this.pageSize() + 1;
  }

  protected last(): number {
    return Math.min(this.total(), this.page() * this.pageSize());
  }

  protected previous(): void {
    if (this.page() > 1) this.pageChange.emit(this.page() - 1);
  }

  protected next(): void {
    if (this.page() < this.pages()) this.pageChange.emit(this.page() + 1);
  }
}

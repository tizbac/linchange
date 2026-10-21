import { ChangeDetectionStrategy, Component, input, output, signal, inject, ElementRef } from '@angular/core';

// The presentational pieces the administration pages share. They are here
// because seven pages that each hand-roll a table header, a banner and a button
// row drift apart within one phase, and a shared spinner is easier to keep
// consistent than seven copies of it.

// PageHeader titles a section and leaves room for the actions on the right.
@Component({
  selector: 'admin-page-header',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex flex-wrap items-end justify-between gap-4 mb-6">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900">{{ title() }}</h1>
        @if (hint()) {
          <p class="mt-1 max-w-2xl text-sm text-slate-500">{{ hint() }}</p>
        }
      </div>
      <div class="flex flex-wrap items-center gap-2"><ng-content /></div>
    </div>
  `,
})
export class PageHeader {
  readonly title = input.required<string>();
  readonly hint = input('');
}

// Panel is the raised card every block of content sits in.
@Component({
  selector: 'admin-panel',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <section class="rounded-2xl bg-white shadow-sm ring-1 ring-slate-200">
      @if (title()) {
        <div class="border-b border-slate-200 px-5 py-3">
          <h2 class="text-sm font-semibold text-slate-900">{{ title() }}</h2>
          @if (hint()) {
            <p class="mt-0.5 text-xs text-slate-500">{{ hint() }}</p>
          }
        </div>
      }
      <div [class]="bodyClass()"><ng-content /></div>
    </section>
  `,
})
export class Panel {
  readonly title = input('');
  readonly hint = input('');
  readonly bodyClass = input('p-5');
}

// Notice is the one place a message is rendered, so a failure never appears as
// an empty table with no explanation.
@Component({
  selector: 'admin-notice',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (message()) {
      <p
        role="status"
        class="rounded-lg px-3 py-2 text-sm ring-1"
        [class]="
          tone() === 'error'
            ? 'bg-rose-50 text-rose-800 ring-rose-200'
            : tone() === 'ok'
              ? 'bg-emerald-50 text-emerald-800 ring-emerald-200'
              : 'bg-slate-50 text-slate-700 ring-slate-200'
        "
      >
        {{ message() }}
      </p>
    }
  `,
})
export class Notice {
  readonly message = input('');
  readonly tone = input<'error' | 'ok' | 'info'>('info');
}

// Busy is shown while a request is in flight so a slow click does not look
// like a page that did nothing.
@Component({
  selector: 'admin-busy',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (busy()) {
      <span class="inline-flex items-center gap-2 text-sm text-slate-500">
        <span
          class="h-4 w-4 animate-spin rounded-full border-2 border-brand-600 border-t-transparent"
          aria-hidden="true"
        ></span>
        {{ label() }}
      </span>
    }
  `,
})
export class Busy {
  readonly busy = input(false);
  readonly label = input('Working…');
}

// PageEmpty distinguishes "nothing exists yet" from "still loading", which a
// bare empty table cannot.
@Component({
  selector: 'admin-empty',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <p class="py-8 text-center text-sm text-slate-500">{{ text() }}</p>
  `,
})
export class PageEmpty {
  readonly text = input('Nothing here yet.');
}

// The button styles live in a component so every action in the panel looks the
// same without each template repeating a dozen utility classes.
@Component({
  selector: 'admin-button',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <button
      [type]="type()"
      [disabled]="disabled()"
      (click)="pressed.emit()"
      [class]="base() + ' ' + (disabled() ? 'cursor-not-allowed opacity-50' : '')"
    >
      <ng-content />
    </button>
  `,
})
export class AdminButton {
  readonly variant = input<'primary' | 'secondary' | 'danger'>('secondary');
  readonly type = input<'button' | 'submit'>('button');
  readonly disabled = input(false);
  readonly pressed = output<void>();

  protected base(): string {
    const shared =
      'inline-flex items-center justify-center gap-1.5 rounded-lg px-3 py-1.5 text-sm font-medium transition';
    switch (this.variant()) {
      case 'primary':
        return `${shared} bg-brand-600 text-white hover:bg-brand-700`;
      case 'danger':
        return `${shared} border border-rose-300 text-rose-700 hover:bg-rose-50`;
      default:
        return `${shared} border border-slate-300 text-slate-700 hover:bg-slate-100`;
    }
  }
}

// Field wraps one labelled control so the label, the control and its hint stay
// associated in one place.
@Component({
  selector: 'admin-field',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <label class="block">
      <span class="block text-xs font-medium text-slate-600">{{ label() }}</span>
      <ng-content />
      @if (hint()) {
        <span class="mt-1 block text-xs text-slate-400">{{ hint() }}</span>
      }
    </label>
  `,
})
export class Field {
  readonly label = input.required<string>();
  readonly hint = input('');
}

export const inputClass =
  'mt-1 w-full rounded-lg border border-slate-300 px-2.5 py-1.5 text-sm text-slate-900 shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500';
export const tableHeadClass = 'px-4 py-2 text-left text-xs font-semibold text-slate-500';
export const tableCellClass = 'px-4 py-2.5 text-sm text-slate-700';

@Component({
  selector: 'admin-multiselect',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="relative">
      <div
        class="min-w-64 flex-1 rounded-lg border border-slate-300 bg-white text-sm text-slate-900 shadow-sm focus-within:border-brand-500 focus-within:ring-1 focus-within:ring-brand-500"
        [class.pointer-events-none]="disabled()"
        (click)="focusInput()"
      >
        <div class="flex flex-wrap items-center gap-1.5 px-2.5 py-1.5 min-h-[38px]">
          @for (m of model(); track m) {
            <span class="inline-flex items-center gap-1 rounded bg-brand-100 text-brand-700 px-2 py-0.5 text-xs">
              {{ m }}
              <button type="button" class="hover:text-brand-900" (click)="removeItem.emit(m); $event.stopPropagation()">×</button>
            </span>
          }
          <input
            #searchInput
            type="text"
            class="flex-1 min-w-[120px] bg-transparent border-none outline-none text-sm text-slate-900 placeholder:text-slate-400"
            [placeholder]="placeholder()"
            [disabled]="disabled()"
            (input)="onInput($event)"
            (keydown)="onKeydown($event)"
            (blur)="onBlur()"
            (focus)="onFocus()"
            autocomplete="off"
          />
        </div>
      </div>
      @if (showDropdown() && filteredOptions().length > 0) {
        <div class="absolute z-10 mt-1 w-full max-h-60 overflow-auto rounded-lg bg-white shadow-lg ring-1 ring-slate-200 animate-in fade-in-0 zoom-in-95 duration-100">
          @for (opt of filteredOptions(); track opt) {
            <button
              type="button"
              class="w-full text-left px-3 py-2 text-sm text-slate-700 hover:bg-slate-100"
              (click)="select(opt)"
            >
              {{ opt }}
            </button>
          }
        </div>
      }
    </div>
  `,
  host: {
    '(document:click)': 'onDocumentClick($event)',
  },
})
export class Multiselect {
  readonly model = input.required<string[]>();
  readonly options = input<string[]>([]);
  readonly placeholder = input('Select...');
  readonly disabled = input(false);
  readonly add = output<string>();
  readonly removeItem = output<string>();
  readonly focus = output<void>();

  protected showDropdown = signal(false);
  protected searchQuery = signal('');
  protected highlightedIndex = signal(-1);
  private readonly elementRef = inject(ElementRef);

  protected filteredOptions(): string[] {
    const q = this.searchQuery().toLowerCase();
    const current = new Set(this.model());
    return this.options()
      .filter(o => !current.has(o) && o.toLowerCase().includes(q))
      .slice(0, 10);
  }

  protected onInput(event: Event): void {
    const value = (event.target as HTMLInputElement).value;
    this.searchQuery.set(value);
    this.showDropdown.set(true);
    this.highlightedIndex.set(-1);
  }

  protected onKeydown(event: KeyboardEvent): void {
    const opts = this.filteredOptions();
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      this.highlightedIndex.update(i => Math.min(i + 1, opts.length - 1));
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      this.highlightedIndex.update(i => Math.max(i - 1, -1));
    } else if (event.key === 'Enter') {
      event.preventDefault();
      if (this.highlightedIndex() >= 0) {
        this.select(opts[this.highlightedIndex()]);
      } else if (this.searchQuery().trim()) {
        this.add.emit(this.searchQuery().trim());
        this.searchQuery.set('');
        this.showDropdown.set(false);
      }
    } else if (event.key === 'Escape') {
      this.showDropdown.set(false);
      this.searchQuery.set('');
      this.highlightedIndex.set(-1);
    }
  }

  protected onFocus(): void {
    this.showDropdown.set(true);
    this.focus.emit();
  }

  protected onBlur(): void {
    setTimeout(() => this.showDropdown.set(false), 150);
  }

  protected select(value: string): void {
    this.add.emit(value);
    this.searchQuery.set('');
    this.showDropdown.set(false);
    this.highlightedIndex.set(-1);
  }

  protected onRemoveItem(value: string): void {
    this.removeItem.emit(value);
  }

  protected focusInput(): void {
    (this.elementRef.nativeElement.querySelector('input') as HTMLInputElement)?.focus();
  }

  protected onDocumentClick(event: MouseEvent): void {
    if (!this.elementRef.nativeElement.contains(event.target)) {
      this.showDropdown.set(false);
    }
  }
}

@Component({
  selector: 'admin-dropdown',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="relative inline-block text-left">
      <button
        type="button"
        class="inline-flex items-center justify-center gap-1.5 rounded-lg border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100"
        [disabled]="disabled()"
        (click)="toggle()"
      >
        <ng-content select="[trigger]">Actions</ng-content>
        <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/></svg>
      </button>
      @if (open()) {
        <div class="absolute right-0 z-10 mt-1 w-48 origin-top-right rounded-lg bg-white shadow-lg ring-1 ring-black ring-opacity-5 focus:outline-none animate-in fade-in-0 zoom-in-95 duration-100">
          <div
            class="py-1"
            role="menu"
            aria-orientation="vertical"
            aria-labelledby="menu-heading"
            (click)="closeMenu()"
          >
            <ng-content />
          </div>
        </div>
      }
    </div>
  `,
  host: {
    '(document:click)': 'onDocumentClick($event)',
  },
})
export class Dropdown {
  readonly disabled = input(false);
  readonly open = signal(false);
  readonly close = output<void>();
  private readonly elementRef = inject(ElementRef);

  toggle(): void {
    if (this.disabled()) return;
    this.open.update(v => !v);
  }

  // closeMenu is the single place the menu goes away. Whoever pressed an item
  // is about to do something else — open a panel, disable a row — and a menu
  // that stays on screen afterwards covers it and cannot be dismissed, because
  // the outside-click path below only ever told the parent, which had nothing
  // listening. The state lives here, so it is cleared here.
  protected closeMenu(): void {
    if (!this.open()) return;
    this.open.set(false);
    this.close.emit();
  }

  protected onDocumentClick(event: MouseEvent): void {
    if (this.open() && !this.elementRef.nativeElement.contains(event.target)) {
      this.closeMenu();
    }
  }
}

@Component({
  selector: 'admin-dropdown-item',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <button
      type="button"
      role="menuitem"
      class="w-full text-left px-3 py-2 text-sm text-slate-700 hover:bg-slate-100"
      [disabled]="disabled()"
      (click)="pressed.emit()"
    >
      <ng-content />
    </button>
  `,
})
export class DropdownItem {
  readonly variant = input<'default' | 'danger'>('default');
  readonly disabled = input(false);
  readonly pressed = output<void>();

  protected base(): string {
    switch (this.variant()) {
      case 'danger':
        return 'text-rose-700 hover:bg-rose-50';
      default:
        return 'text-slate-700 hover:bg-slate-100';
    }
  }
}

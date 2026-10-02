import { ChangeDetectionStrategy, Component, input, output } from '@angular/core';

// The presentational pieces the administration pages share. They are here
// because seven pages that each hand-roll a table header, a banner and a button
// row drift apart within one phase, and a shared spinner is easier to keep
// consistent than seven copies of it.

// PageHeader titles a section and leaves room for the actions on the right.
@Component({
  selector: 'admin-page-header',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex flex-wrap items-end justify-between gap-4">
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

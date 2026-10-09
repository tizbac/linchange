import { ChangeDetectionStrategy, Component, input } from '@angular/core';

@Component({
  selector: 'admin-page',
  changeDetection: ChangeDetectionStrategy.OnPush,
  imports: [],
  template: `
    <div class="mx-auto max-w-5xl px-6 py-6">
      <ng-content />
    </div>
  `,
})
export class AdminPageComponent {
  readonly title = input<string>('');
}
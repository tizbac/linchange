import { AfterViewInit, ChangeDetectionStrategy, Component, OnDestroy, ViewChild, ViewEncapsulation, effect, inject, input, output, signal } from '@angular/core';
import { EditorComponent, TINYMCE_SCRIPT_SRC } from '@tinymce/tinymce-angular';
import type { Editor as TinyMCEEditor } from 'tinymce';
import { AuthService } from '../auth/auth.service';

// The HTML body editor.
//
// The body is edited as rich text and stored as HTML, because that is what a
// mail client is expected to render and what the recipient's client will show.
// A draft that carried HTML markup as literal text was the alternative, and it
// is not what anybody means to write.
//
// Everything a user can do here is a formatting command and none of them change
// what the message says, so the editor is deliberately small: no tables, no
// images, no arbitrary HTML pasted in. The HTML it produces is a fixed set of
// elements, which is also what makes it safe to hand to a recipient's client.
//
// The editor writes out through htmlChange rather than a bound model, because
// the parent needs to know when the body changed, not to re-drive the editor:
// writing back into the editor on every keystroke would reset the caret.

// htmlToText turns an HTML body into the plain-text alternative.
//
// Both parts are sent, because a message is multipart/alternative and a client
// that cannot render HTML still has to be able to read something. The conversion
// is done here rather than on the server so the composer can show the user the
// plain text their recipient will see if they ask for it.
//
// It walks the DOM rather than using innerText on the editor, because
// innerText of the editable element depends on layout that is not the same once
// the HTML has been sent.
export function htmlToText(html: string): string {
  if (!html) return '';
  const doc = new DOMParser().parseFromString(html, 'text/html');
  return domToText(doc.body);
}

function domToText(node: Node): string {
  // A <br> is a newline inside a paragraph but nothing at all between two
  // blocks, which is why the block elements below add their own.
  if (node.nodeType === Node.TEXT_NODE) {
    return node.nodeValue ?? '';
  }
  if (node.nodeType !== Node.ELEMENT_NODE) {
    return Array.from(node.childNodes).map(domToText).join('');
  }
  const el = node as HTMLElement;
  switch (el.tagName) {
    case 'BR':
      return '\n';
    case 'P':
    case 'DIV':
    case 'H1':
    case 'H2':
    case 'H3':
    case 'H4':
    case 'LI':
    case 'BLOCKQUOTE':
    case 'PRE':
      return `${blockChildren(el)}\n`;
    case 'UL':
    case 'OL':
      return `${blockChildren(el)}\n`;
    case 'A':
      // The address is in the text already for a link whose text is the
      // address, and repeating it for one whose text is a title helps nobody.
      return domChildren(el);
    case 'HR':
      return '\n---\n';
    case 'IMG':
      // An image has no plain-text equivalent, and an empty marker tells the
      // reader something was there.
      return el.getAttribute('alt') ? `[${el.getAttribute('alt')}]` : '';
    // Inline formatting: strong/b, em/i, u, s, mark - just render their text content
    case 'STRONG':
    case 'B':
    case 'EM':
    case 'I':
    case 'U':
    case 'S':
    case 'STRIKE':
    case 'MARK':
    case 'SPAN':
      return domChildren(el);
    default:
      return domChildren(el);
  }
}

function blockChildren(el: HTMLElement): string {
  return Array.from(el.childNodes).map(domToText).join('');
}

function domChildren(el: HTMLElement): string {
  return Array.from(el.childNodes).map(domToText).join('');
}

// htmlHasContent reports whether an HTML body says anything.
//
// The editor emits a paragraph element for an empty document, so a body that
// looks empty to the eye is not empty as a string, and asking "does this message
// have a body" by testing the markup would pass a message that says nothing.
export function htmlHasContent(html: string): boolean {
  if (!html) return false;
  const doc = new DOMParser().parseFromString(html, 'text/html');
  // An image counts: a message whose only content is a picture has a body.
  if (doc.body.querySelector('img')) return true;
  return (doc.body.textContent ?? '').trim() !== '';
}

// Provide TinyMCE script source for self-hosted deployment
export const provideTinyMCE = () => ({
  provide: TINYMCE_SCRIPT_SRC,
  useValue: '/tinymce/tinymce.min.js',
});

// Component: rich-text-editor
@Component({
  selector: 'app-rich-text-editor',
  changeDetection: ChangeDetectionStrategy.OnPush,
  encapsulation: ViewEncapsulation.None,
  template: `
    <editor
      #tiny
      [init]="init()"
      [initialValue]="html()"
      [disabled]="!showToolbar()"
      (onChange)="onChange($event)"
      (onInit)="onInit($event)"
    ></editor>
  `,
  styles: [
    `
      /* Host element gets height so the editor fills it */
      :host {
        display: block;
        min-height: 16rem;
      }
    `,
  ],
  imports: [EditorComponent],
})
export class RichTextEditorComponent implements AfterViewInit, OnDestroy {
  // html is the body to show. It is an input rather than a model so the parent
  // can set it once, for a reply, without the editor writing back on every
  // keystroke and fighting the caret.
  readonly html = input<string>('');
  // htmlChange fires when the body changes, with the HTML.
  readonly htmlChange = output<string>();
  // showToolbar is false for a read-only preview.
  readonly showToolbar = input<boolean>(true);
  // placeholder is what an empty editor shows.
  readonly placeholder = input<string>('Write your message');
  // uploadEndpoint is the API endpoint to upload pasted images to.
  // 'signature' -> /api/v1/mail/images/signature
  // 'compose'   -> /api/v1/mail/images/compose
  readonly uploadEndpoint = input<'signature' | 'compose'>('compose');

  @ViewChild('tiny') tinyRef!: EditorComponent;

  private editorInstance: TinyMCEEditor | null = null;
  private dirty = false;
  private lastEmitted = '';
  private initialized = false;

  private readonly auth = inject(AuthService);

  readonly init = signal(this.initOptions());

  constructor() {
    effect(() => {
      // The skin and the content stylesheet are chosen at init. Updating the
      // signal also reinitialises the editor if it is already on screen, so a
      // theme change while composing does not leave a light iframe behind.
      this.placeholder();
      this.auth.themeKind();
      this.init.set(this.initOptions());
    });
  }

  private initOptions() {
    const kind = this.auth.themeKind();
    const dark =
      kind === 'dark' ||
      (kind === 'system' &&
        typeof window !== 'undefined' &&
        !!window.matchMedia?.('(prefers-color-scheme: dark)')?.matches);
    return {
      height: 400,
      menubar: false,
      // The oxide-dark skin is what makes the toolbar and menu chrome match the
      // rest of the application; content_css colours the editable iframe.
      skin: dark ? 'oxide-dark' : 'oxide',
      content_css: dark ? 'dark' : 'default',
      toolbar: 'undo redo | blocks | bold italic underline strikethrough forecolor backcolor | alignleft aligncenter alignright | bullist numlist outdent indent | blockquote | link | image',
      plugins: 'link lists image',
      content_style: dark
        ? `
      body {
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
        font-size: 0.875rem;
        line-height: 1.625;
        background: #1e293b;
        color: #e2e8f0;
        padding: 0.5rem 0.75rem;
      }
      p { margin: 0 0 0.5rem; }
      p:last-child { margin-bottom: 0; }
      ul, ol { margin: 0 0 0.5rem 1.25rem; }
      ul { list-style: disc; }
      ol { list-style: decimal; }
      blockquote { margin: 0 0 0.5rem; border-left: 3px solid #475569; padding-left: 0.75rem; color: #cbd5e1; }
      h1, h2, h3 { margin: 0 0 0.5rem; font-weight: 600; }
      h1 { font-size: 1.25rem; }
      h2 { font-size: 1.125rem; }
      h3 { font-size: 1rem; }
      pre { margin: 0 0 0.5rem; border-radius: 0.375rem; background: #0f172a; padding: 0.5rem; font-family: ui-monospace, monospace; font-size: 0.8125rem; }
      code { font-family: ui-monospace, monospace; font-size: 0.875em; }
      a { color: #93c5fd; text-decoration: underline; }
    `
        : `
      body {
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
        font-size: 0.875rem;
        line-height: 1.625;
        color: #1e293b;
        padding: 0.5rem 0.75rem;
      }
      p { margin: 0 0 0.5rem; }
      p:last-child { margin-bottom: 0; }
      ul, ol { margin: 0 0 0.5rem 1.25rem; }
      ul { list-style: disc; }
      ol { list-style: decimal; }
      blockquote { margin: 0 0 0.5rem; border-left: 3px solid #cbd5e1; padding-left: 0.75rem; color: #475569; }
      h1, h2, h3 { margin: 0 0 0.5rem; font-weight: 600; }
      h1 { font-size: 1.25rem; }
      h2 { font-size: 1.125rem; }
      h3 { font-size: 1rem; }
      pre { margin: 0 0 0.5rem; border-radius: 0.375rem; background: #f1f5f9; padding: 0.5rem; font-family: ui-monospace, monospace; font-size: 0.8125rem; }
      code { font-family: ui-monospace, monospace; font-size: 0.875em; }
      a { color: #2563eb; text-decoration: underline; }
    `,
      placeholder: this.placeholder(),
      link_assume_external_targets: 'https',
      link_default_protocol: 'https',
      // Use GPL license for self-hosted deployment
      license_key: 'gpl',
      // Block formats for the 'blocks' toolbar button
      block_formats: 'Paragraph=p; Heading 1=h1; Heading 2=h2; Heading 3=h3; Blockquote=blockquote; Preformatted=pre',
      // Allow images to be pasted directly (base64, converted by server on send)
      paste_data_images: true,
    };
  }

  ngAfterViewInit(): void {
    // Wait for onInit to get the editor instance
  }

  ngOnDestroy(): void {
    this.editorInstance?.destroy();
  }

  onInit(event: { editor: TinyMCEEditor }): void {
    this.editorInstance = event.editor;
    this.initialized = true;
    // Set initial content if not already set
    if (this.html() && this.editorInstance.getContent() !== this.html()) {
      this.editorInstance.setContent(this.html());
    }
    this.lastEmitted = this.editorInstance.getContent();
  }

  onChange(event: { editor: TinyMCEEditor }): void {
    if (!this.initialized) return;

    const editor = event.editor;
    this.editorInstance = editor;

    const content = editor.getContent();
    if (content !== this.lastEmitted) {
      this.lastEmitted = content;
      this.htmlChange.emit(content);
    }
  }
}
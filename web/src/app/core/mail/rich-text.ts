import { AfterViewInit, ChangeDetectionStrategy, Component, ElementRef, OnDestroy, effect, input, output, viewChild } from '@angular/core';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';
import Link from '@tiptap/extension-link';

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

// Component: rich-text-editor
@Component({
  selector: 'app-rich-text-editor',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="overflow-hidden rounded-lg border border-slate-300 focus-within:border-brand-500">
      @if (showToolbar()) {
        <div
          class="flex flex-wrap items-center gap-1 border-b border-slate-200 bg-slate-50 px-2 py-1.5"
          role="toolbar"
          aria-label="Formatting"
        >
          <button
            type="button"
            class="rounded px-2 py-1 text-xs font-semibold text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('bold')"
            [attr.aria-pressed]="isActive('bold')"
            title="Bold"
            (click)="toggleBold()"
          >
            B
          </button>
          <button
            type="button"
            class="rounded px-2 py-1 text-xs italic text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('italic')"
            [attr.aria-pressed]="isActive('italic')"
            title="Italic"
            (click)="toggleItalic()"
          >
            I
          </button>
          <button
            type="button"
            class="rounded px-2 py-1 text-xs underline text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('underline')"
            [attr.aria-pressed]="isActive('underline')"
            title="Underline"
            (click)="toggleUnderline()"
          >
            U
          </button>
          <button
            type="button"
            class="rounded px-2 py-1 text-xs text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('strike')"
            [attr.aria-pressed]="isActive('strike')"
            title="Strikethrough"
            (click)="toggleStrike()"
          >
            S
          </button>

          <span class="mx-1 h-4 w-px bg-slate-300"></span>

          <button
            type="button"
            class="rounded px-2 py-1 text-xs text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('bulletList')"
            [attr.aria-pressed]="isActive('bulletList')"
            title="Bulleted list"
            (click)="toggleBulletList()"
          >
            &bull; List
          </button>
          <button
            type="button"
            class="rounded px-2 py-1 text-xs text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('orderedList')"
            [attr.aria-pressed]="isActive('orderedList')"
            title="Numbered list"
            (click)="toggleOrderedList()"
          >
            1. List
          </button>
          <button
            type="button"
            class="rounded px-2 py-1 text-xs text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('blockquote')"
            [attr.aria-pressed]="isActive('blockquote')"
            title="Quoted text"
            (click)="toggleBlockquote()"
          >
            &ldquo;
          </button>

          <span class="mx-1 h-4 w-px bg-slate-300"></span>

          <button
            type="button"
            class="rounded px-2 py-1 text-xs text-slate-700 transition hover:bg-slate-200"
            [class.bg-slate-200]="isActive('link')"
            [attr.aria-pressed]="isActive('link')"
            title="Insert or edit a link"
            (click)="setLink()"
          >
            Link
          </button>
          @if (isActive('link')) {
            <button
              type="button"
              class="rounded px-2 py-1 text-xs text-rose-700 transition hover:bg-rose-50"
              title="Remove the link"
              (click)="unsetLink()"
            >
              Unlink
            </button>
          }
        </div>
      }
      <div
        #surface
        class="prose-editor min-h-64 px-3 py-2 text-sm leading-relaxed text-slate-900 focus:outline-none"
        data-placeholder="Write your message"
      ></div>
    </div>
  `,
  styles: [
    `
      /* The editable surface is styled here rather than through a utility class
         because ProseMirror manages its own content, and the child elements it
         creates are not visible to the template. */
      .prose-editor:empty::before {
        content: attr(data-placeholder);
        color: #94a3b8;
      }
      .prose-editor p {
        margin: 0 0 0.5rem;
      }
      .prose-editor p:last-child {
        margin-bottom: 0;
      }
      .prose-editor ul,
      .prose-editor ol {
        margin: 0 0 0.5rem 1.25rem;
      }
      .prose-editor ul {
        list-style: disc;
      }
      .prose-editor ol {
        list-style: decimal;
      }
      .prose-editor blockquote {
        margin: 0 0 0.5rem;
        border-left: 3px solid #cbd5e1;
        padding-left: 0.75rem;
        color: #475569;
      }
      .prose-editor h1,
      .prose-editor h2,
      .prose-editor h3 {
        margin: 0 0 0.5rem;
        font-weight: 600;
      }
      .prose-editor h1 {
        font-size: 1.25rem;
      }
      .prose-editor h2 {
        font-size: 1.125rem;
      }
      .prose-editor h3 {
        font-size: 1rem;
      }
      .prose-editor pre {
        margin: 0 0 0.5rem;
        border-radius: 0.375rem;
        background: #f1f5f9;
        padding: 0.5rem;
        font-family: ui-monospace, monospace;
        font-size: 0.8125rem;
      }
      .prose-editor code {
        font-family: ui-monospace, monospace;
        font-size: 0.875em;
      }
      .prose-editor a {
        color: #2563eb;
        text-decoration: underline;
      }
      .ProseMirror-focused {
        outline: none;
      }
      .ProseMirror-selectednode {
        outline: 2px solid #3b82f6;
      }
    `,
  ],
  imports: [],
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

  private readonly surface = viewChild.required<ElementRef<HTMLElement>>('surface');

  private editor: Editor | null = null;

  // dirty records whether the body was changed by the user, so an incoming
  // value is only written into the editor when it is not the value the editor
  // itself produced. Without it, the parent's own echo of a keystroke would
  // reset the cursor to the start of the document.
  private dirty = false;

  constructor() {
    // The placeholder is an attribute rather than a ProseMirror plugin so that
    // it needs no editor instance to exist: it is read by CSS when the editor
    // is empty, which is a state the DOM already reports.
    effect(() => {
      const el = this.surface().nativeElement;
      el.setAttribute('data-placeholder', this.placeholder());
    });
  }

  ngAfterViewInit(): void {
    this.editor = new Editor({
      element: this.surface().nativeElement,
      extensions: [StarterKit, Link.configure({ openOnClick: false })],
      content: this.html(),
      // The editor's own state is the truth for the body, so a keystroke emits
      // the HTML and nothing writes back into it.
      onUpdate: () => {
        this.dirty = true;
        this.htmlChange.emit(this.editor?.getHTML() ?? '');
      },
    });
  }

  ngOnDestroy(): void {
    this.editor?.destroy();
    this.editor = null;
  }

  // isActive reports whether a mark or node is applied at the caret, so a button
  // can show itself as pressed. FullCalendar-style: the toolbar reflects the
  // text the user is in, not the document as a whole.
  protected isActive(name: string): boolean {
    return this.editor?.isActive(name) ?? false;
  }

  protected toggleBold(): void {
    this.editor?.chain().focus().toggleBold().run();
  }

  protected toggleItalic(): void {
    this.editor?.chain().focus().toggleItalic().run();
  }

  protected toggleUnderline(): void {
    this.editor?.chain().focus().toggleUnderline().run();
  }

  protected toggleStrike(): void {
    this.editor?.chain().focus().toggleStrike().run();
  }

  protected toggleBulletList(): void {
    this.editor?.chain().focus().toggleBulletList().run();
  }

  protected toggleOrderedList(): void {
    this.editor?.chain().focus().toggleOrderedList().run();
  }

  protected toggleBlockquote(): void {
    this.editor?.chain().focus().toggleBlockquote().run();
  }

  // setLink asks for the address and applies it. An empty answer removes the
  // link, which is what a user pressing Cancel after selecting text expects.
  protected setLink(): void {
    const existing = this.editor?.getAttributes('link')['href'] as string | undefined;
    const url = window.prompt('Link address', existing ?? 'https://');
    if (url === null) return;
    const trimmed = url.trim();
    if (!trimmed) {
      this.unsetLink();
      return;
    }
    // A link the recipient cannot use is worse than no link: javascript: and
    // data: URLs are executed by some clients and mean nothing in an email.
    if (!/^https?:\/\//i.test(trimmed)) {
      window.alert('A link has to start with http:// or https://');
      return;
    }
    this.editor?.chain().focus().extendMarkRange('link').setLink({ href: trimmed }).run();
  }

  protected unsetLink(): void {
    this.editor?.chain().focus().extendMarkRange('link').unsetLink().run();
  }
}
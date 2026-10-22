import { Injectable } from '@angular/core';
import { Title } from '@angular/platform-browser';
import { RouterStateSnapshot, TitleStrategy } from '@angular/router';

// Every page gets a window title of the form "Linchange - Mail". The section
// is the deepest route title; those titles are marked for translation, so the
// Italian build reads "Linchange - Posta" with the same brand prefix.
@Injectable({ providedIn: 'root' })
export class AppTitleStrategy extends TitleStrategy {
  constructor(private readonly title: Title) {
    super();
  }

  override updateTitle(snapshot: RouterStateSnapshot): void {
    const page = this.buildTitle(snapshot);
    this.title.setTitle(page ? `Linchange - ${page}` : 'Linchange');
  }
}

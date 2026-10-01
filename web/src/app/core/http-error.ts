import { HttpErrorResponse } from '@angular/common/http';

// Turns a thrown value into a short, human-readable message for a form. The API
// sends { "error": "..." } on failure, so that is preferred when present.
export function errorMessage(err: unknown): string {
  if (err instanceof HttpErrorResponse) {
    const body = err.error as { error?: string } | undefined;
    if (body && typeof body.error === 'string' && body.error.length > 0) {
      return body.error;
    }
    return 'Something went wrong. Please try again.';
  }
  if (err instanceof Error && err.message) {
    return err.message;
  }
  return 'Something went wrong. Please try again.';
}

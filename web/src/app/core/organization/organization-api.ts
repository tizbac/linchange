import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';

const API = '/api/v1/organization';

// Contact is one person in the organization: an address that accepts mail and
// the display name its mailbox carries.
export interface Contact {
  address: string;
  display_name?: string;
}

// OrganizationApi is the only place that names the organization endpoints. The
// organization is derived from the caller's own address on the server, so there
// is nothing to pass in and no way to ask for another one.
@Injectable({ providedIn: 'root' })
export class OrganizationApi {
  private readonly http = inject(HttpClient);

  // people lists every mailbox in the domains the signed-in address belongs to.
  people(): Promise<{ people: Contact[] }> {
    return firstValueFrom(this.http.get<{ people: Contact[] }>(`${API}/people`));
  }
}

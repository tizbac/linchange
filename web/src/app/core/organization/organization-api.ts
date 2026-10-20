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

// OrganizationGroup is a named set of people in the caller's organization. A
// client never expands it into addresses: the server does that when the group
// is invited to a meeting or shared a calendar with, so all it needs here is
// the id and the label it goes by.
export interface OrganizationGroup {
  id: number;
  workgroup_id: number;
  name: string;
  description?: string;
  // everyone marks the organization's own group — every address in its
  // domains — whose membership follows the directory rather than a list.
  everyone?: boolean;
  member_count: number;
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

  // groups lists the groups of the caller's organization. Somebody whose domain
  // belongs to no organization gets an empty list, which is also the truth.
  groups(): Promise<{ groups: OrganizationGroup[] }> {
    return firstValueFrom(this.http.get<{ groups: OrganizationGroup[] }>(`${API}/groups`));
  }
}

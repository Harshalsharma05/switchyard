// Identity calls. fetchMe validates a key and returns the team it belongs to;
// the auth layer uses a successful response as proof the key is good.
import { request } from './client.js'

export function fetchMe(key, signal) {
  return request('/admin/me', { key, signal })
}

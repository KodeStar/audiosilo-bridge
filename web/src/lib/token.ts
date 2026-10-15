// The session token lives in localStorage so a page reload keeps the session.
export const TOKEN_KEY = 'audiosilo_bridge_token';

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string): void {
  try {
    localStorage.setItem(TOKEN_KEY, token);
  } catch {
    // Storage blocked (private mode): the in-memory state still holds it.
  }
}

export function clearToken(): void {
  try {
    localStorage.removeItem(TOKEN_KEY);
  } catch {
    // Ignore.
  }
}

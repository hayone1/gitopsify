declare module '#app' {
  interface PageMeta {
    // The server-wide role a page needs (see middleware/auth.global.ts).
    role?: 'viewer' | 'operator' | 'admin'
  }
}

export {}

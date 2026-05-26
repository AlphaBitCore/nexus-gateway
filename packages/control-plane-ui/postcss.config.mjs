// PostCSS resolves `@import "tailwindcss"` in `src/styles/tailwind-app.css` for Vite,
// Vitest, and Storybook. Without `@tailwindcss/postcss`, plain PostCSS treats
// `tailwindcss` as a filesystem path → ENOENT.
export default {
  plugins: {
    '@tailwindcss/postcss': {},
  },
};

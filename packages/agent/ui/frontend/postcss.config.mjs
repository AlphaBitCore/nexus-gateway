// Same as control-plane-ui: Tailwind v4 entry `@import "tailwindcss"` needs this plugin
// whenever PostCSS processes CSS (Vite dev/build, Wails embed build).
export default {
  plugins: {
    '@tailwindcss/postcss': {},
  },
};

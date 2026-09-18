import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    proxy: {
      '/api': 'http://localhost:8090',
      '/install.sh': 'http://localhost:8090',
      '/agent': { target: 'ws://localhost:8090', ws: true }
    }
  },
  test: { include: ['src/**/*.test.ts'] }
});

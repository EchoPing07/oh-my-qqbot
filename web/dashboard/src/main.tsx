import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'

import './index.css'
import './i18n'
import { AnnouncerProvider } from './components/ui/announcer'
import { ThemeProvider } from './components/theme-provider'
import { ErrorBoundary } from './components/error-boundary'
import { Toaster } from './components/ui/toaster'
import { queryClient } from './lib/query'
import { router } from './router'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <AnnouncerProvider>
          <ThemeProvider defaultTheme="system">
            <RouterProvider router={router} />
            <Toaster />
          </ThemeProvider>
        </AnnouncerProvider>
      </QueryClientProvider>
    </ErrorBoundary>
  </StrictMode>
)

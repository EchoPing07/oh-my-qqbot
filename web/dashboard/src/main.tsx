import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'

import './index.css'

import { MenuProvider, Toaster } from '@/components/ui'
import { ErrorBoundary } from '@/components/ErrorBoundary'
import { queryClient } from '@/lib/query'
import { router } from '@/router'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <MenuProvider>
          <RouterProvider router={router} />
          <Toaster />
        </MenuProvider>
      </QueryClientProvider>
    </ErrorBoundary>
  </StrictMode>
)

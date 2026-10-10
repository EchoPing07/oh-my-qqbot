import { cn } from '@/lib/utils'

import { type ThemeOptionProps } from './types'

export function ThemeOption({ value, current, onChange, label, description }: ThemeOptionProps) {
  const isSelected = current === value

  return (
    <button
      onClick={() => onChange(value)}
      className={cn(
        'relative rounded-lg border-2 p-3 sm:p-4 text-left transition-all',
        'hover:border-primary/50 hover:bg-accent/50',
        isSelected ? 'border-primary bg-accent' : 'border-border'
      )}
    >
      {isSelected && (
        <div className="absolute top-2 right-2 sm:top-3 sm:right-3 h-2 w-2 rounded-full bg-primary" />
      )}

      <div className="space-y-1">
        <div className="text-sm sm:text-base font-medium">{label}</div>
        <div className="text-[10px] sm:text-xs text-muted-foreground">{description}</div>
      </div>

      <div className="mt-2 flex gap-1 sm:mt-3">
        {value === 'light' && (
          <>
            <div className="h-2 w-2 border-border border bg-background" />
            <div className="h-2 w-2 border-border border bg-muted" />
            <div className="h-2 w-2 border-border border bg-accent" />
          </>
        )}
        {value === 'dark' && (
          <>
            <div className="h-2 w-2 border-border border bg-foreground/70" />
            <div className="h-2 w-2 border-border border bg-foreground/85" />
            <div className="h-2 w-2 border-border border bg-foreground" />
          </>
        )}
        {value === 'system' && (
          <>
            <div className="h-2 w-2 border-border border bg-background" />
            <div className="h-2 w-2 border-border border bg-muted" />
            <div className="h-2 w-2 border-border border bg-foreground" />
          </>
        )}
      </div>
    </button>
  )
}

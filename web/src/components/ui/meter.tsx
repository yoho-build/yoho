import { Meter as MeterPrimitive } from "@base-ui/react/meter"

import { cn } from "@/lib/utils"

function Meter({ value, label, detail, className }: { value: number; label: string; detail?: string; className?: string }) {
  const v = Math.max(0, Math.min(100, Number.isFinite(value) ? value : 0))
  const tone = v >= 90 ? "bg-destructive" : v >= 75 ? "bg-warning" : "bg-primary"
  return (
    <MeterPrimitive.Root value={v} className={cn("flex flex-col gap-1.5", className)} aria-label={label}>
      <div className="flex items-baseline justify-between gap-2 text-xs">
        <MeterPrimitive.Label className="font-medium">{label}</MeterPrimitive.Label>
        <span className="text-muted-foreground tabular-nums">{detail ?? `${v.toFixed(0)}%`}</span>
      </div>
      <MeterPrimitive.Track className="h-1.5 overflow-hidden rounded-full bg-muted">
        <MeterPrimitive.Indicator className={cn("h-full rounded-full transition-all", tone)} />
      </MeterPrimitive.Track>
    </MeterPrimitive.Root>
  )
}

export { Meter }

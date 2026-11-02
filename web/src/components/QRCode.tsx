import { useMemo } from 'react'
import { encode } from 'uqr'

/** A QR code rendered locally as SVG (the otpauth URI never leaves the browser). */
export function QRCode({ value, size = 176, label }: { value: string; size?: number; label: string }) {
  const path = useMemo(() => {
    const qr = encode(value, { ecc: 'M', border: 2 })
    let d = ''
    qr.data.forEach((row, y) =>
      row.forEach((on, x) => {
        if (on) d += `M${x} ${y}h1v1h-1z`
      }),
    )
    return { d, n: qr.size }
  }, [value])
  return (
    <svg role="img" aria-label={label} width={size} height={size} viewBox={`0 0 ${path.n} ${path.n}`} shapeRendering="crispEdges"
      style={{ background: '#fff', borderRadius: 4 }}>
      <path d={path.d} fill="#000" />
    </svg>
  )
}

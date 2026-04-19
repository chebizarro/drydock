# Drydock AI SVG-ready vector specs

## Brand intent
A converging, abstract fleet mark that suggests coordinated agents, event flow, and dock-like structure.

## Palette
- Deep Navy: `#071B3A`
- Signal Cyan: `#1EC8F3`
- Dock Blue: `#178FE6`
- Ice White: `#E8EEF7`
- Steel Mist: `#AFC5DB`

## Master symbol
- Artboard: `256 × 180`
- Safe area: `16` units on all sides
- Visual center: `128, 96`
- Construction: six faceted polygons arranged around a central vertical seam

## Geometry map
1. Left outer wing: `18,110 70,110 106,138 54,138`
2. Left inner wing: `78,78 116,106 116,150 78,122`
3. Center left blade: `110,46 142,70 142,146 110,124`
4. Center right blade: `146,46 178,22 178,100 146,124`
5. Right inner wing: `142,106 180,78 180,122 142,150`
6. Right outer wing: `150,110 202,110 238,110 202,138 150,138`

## Recommended versions
- Primary symbol: 6-shape faceted mark
- Favicon micro: same silhouette with flatter fills and no fine effects
- App icon: symbol centered in rounded square, corner radius about 21% of canvas

## SVG implementation notes
- Use polygons for crisp edges and easy editing
- Avoid strokes in the default version
- Use gradients sparingly; flat-color fallback should always exist
- Keep minimum symbol size above `14px` tall for browser tabs

## File pack
- `drydock-symbol.svg`
- `drydock-app-icon.svg`
- `drydock-favicon.svg`

## Suggested usage
- Dark UI: navy background with cyan/light symbol
- Light UI: invert to white background with navy/cyan symbol
- Status variants can tint the cyan planes:
  - Review: amber
  - Pass: green
  - Alert: red

package raycaster

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"

	"github.com/harbdog/raycaster-go/geom"

	"github.com/hajimehoshi/ebiten/v2"
)

const (
	// maximum number of concurrent tasks for large task sets (e.g. floor and sprite casting)
	maxConcurrent = 100

	// distance to keep camera away from edge of game world to avoid index exception
	edgeDistance = 0.1
)

type MouseMode int

const (
	MouseModeLook MouseMode = iota
	MouseModeMove
	MouseModeCursor
)

// Camera Class that represents a camera in terms of raycasting.
// Contains methods to move the camera, and handles projection to,
// set the rectangle slice position and height,
type Camera struct {
	//--camera position, init to start position--//
	pos *geom.Vector2

	// vertical camera strafing up/down, for jumping/crouching
	posZ float64

	//--current facing direction, init to values coresponding to FOV--//
	dir *geom.Vector2

	//--the 2d raycaster version of camera plane, adjust y component to change FOV (ratio between this and dir x resizes FOV)--//
	plane *geom.Vector2

	//--viewport width and height--//
	w int
	h int

	// camera pitch
	pitch int

	// camera fov angle and depth
	fovAngle, fovDepth float64

	// target framerate reference
	targetTPS int

	//--world map--//
	mapObj    Map
	mapWidth  int
	mapHeight int

	//--test texture--//
	floor *ebiten.Image
	sky   *ebiten.Image

	//--texture width--//
	texSize int

	//--structs that contain rects and tints for each level render--//
	levels []*level

	// zbuffer for sprite casting
	zBuffer []float64
	// sprites
	sprites []Sprite
	//arrays used to sort the sprites
	spriteOrder    []int
	spriteDistance []float64

	spriteLvls []*level
	tex        *TextureHandler

	floorLvl *horLevel

	// used for concurrency
	semaphore chan struct{}
}

// NewCamera initalizes a Camera object
func NewCamera(width int, height int, texSize int, mapObj Map, tex *TextureHandler) *Camera {

	fmt.Printf("Initializing Camera\n")

	c := &Camera{}

	// set target TPS
	// TODO: make target FPS customizable
	c.targetTPS = 60
	ebiten.SetMaxTPS(c.targetTPS)

	//--camera position, init to some start position--//
	fovDegrees := 70.0
	c.fovAngle = geom.Radians(fovDegrees)
	c.fovDepth = 1.0
	c.pos = &geom.Vector2{X: 1.0, Y: 1.0}
	c.posZ = 0.0

	//--current facing direction, init to some start position--//
	c.dir = c.GetVecForAngle(0)

	//--the 2d raycaster version of camera plane (ratio between this and dir x resizes FOV)--//
	c.plane = c.GetVecForFov(c.dir)

	c.w = width
	c.h = height
	c.pitch = 0
	c.texSize = texSize
	c.tex = tex

	c.levels = c.createLevels(mapObj.NumLevels())
	c.floorLvl = c.createFloorLevel()

	// set zbuffer based on screen width
	c.zBuffer = make([]float64, width)

	c.mapObj = mapObj
	firstLevel := mapObj.Level(0)

	c.mapWidth = len(firstLevel)
	c.mapHeight = len(firstLevel)

	c.sprites = []Sprite{}
	c.updateSpriteLevels(16)

	// initialize a pool of channels to limit concurrent floor and sprite casting
	// from https://pocketgophers.com/limit-concurrent-use/
	c.semaphore = make(chan struct{}, maxConcurrent)

	//do an initial raycast
	c.raycast()

	return c
}

func (c *Camera) SetFloorTexture(floor *ebiten.Image) {
	c.floor = floor
}

func (c *Camera) SetSkyTexture(sky *ebiten.Image) {
	c.sky = sky
}

// Update - updates the camera view
func (c *Camera) Update(sprites []Sprite) {
	// clear horizontal buffer by making a new one
	c.floorLvl.clear(c.w, c.h)

	if len(sprites) != len(c.sprites) {
		// sprite buffer may need to be increased in size
		c.updateSpriteLevels(len(sprites))
	} else {
		c.clearAllSpriteLevel()
	}

	//--do raycast--//
	c.sprites = sprites
	c.raycast()
}

func (c *Camera) raycast() {
	// cast level
	numLevels := c.mapObj.NumLevels()
	var wg sync.WaitGroup
	for i := 0; i < numLevels; i++ {
		wg.Add(1)
		go c.asyncCastLevel(i, &wg)
	}

	wg.Wait()

	//SPRITE CASTING
	numSprites := len(c.sprites)
	c.spriteOrder = make([]int, numSprites)
	c.spriteDistance = make([]float64, numSprites)
	//sort sprites from far to close
	for i := 0; i < numSprites; i++ {
		sprite := c.sprites[i]
		c.spriteOrder[i] = i
		c.spriteDistance[i] = (math.Pow(c.pos.X-sprite.Pos().X, 2) + math.Pow(c.pos.Y-sprite.Pos().Y, 2))
	}
	combSort(c.spriteOrder, c.spriteDistance, numSprites)

	//after sorting the sprites, do the projection and draw them
	for i := 0; i < numSprites; i++ {
		wg.Add(1)
		go c.asyncCastSprite(i, &wg)
	}

	wg.Wait()
}

func (c *Camera) asyncCastLevel(levelNum int, wg *sync.WaitGroup) {
	defer wg.Done()

	rMap := c.mapObj.Level(levelNum)

	for x := 0; x < c.w; x++ {
		c.castLevel(x, rMap, c.levels[levelNum], levelNum, wg)
	}
}

func (c *Camera) asyncCastSprite(spriteNum int, wg *sync.WaitGroup) {
	defer wg.Done()

	c.semaphore <- struct{}{} // Lock
	defer func() {
		<-c.semaphore // Unlock
	}()

	c.castSprite(spriteNum)
}

// credit : Raycast loop and setting up of vectors for matrix calculations
// courtesy - http://lodev.org/cgtutor/raycasting.html
func (c *Camera) castLevel(x int, grid [][]int, lvl *level, levelNum int, wg *sync.WaitGroup) {
	var _cts, _sv []*image.Rectangle
	var _st []*color.RGBA

	_cts = lvl.Cts
	_sv = lvl.Sv
	_st = lvl.St

	//calculate ray position and direction
	cameraX := 2.0*float64(x)/float64(c.w) - 1.0 //x-coordinate in camera space
	rayDirX := c.dir.X + c.plane.X*cameraX
	rayDirY := c.dir.Y + c.plane.Y*cameraX

	//--rays start at camera position--//
	rayPosX := c.pos.X
	rayPosY := c.pos.Y

	//which box of the map we're in
	mapX := int(rayPosX)
	mapY := int(rayPosY)

	//length of ray from current position to next x or y-side
	var sideDistX float64
	var sideDistY float64

	//length of ray from one x or y-side to next x or y-side
	deltaDistX := math.Abs(1 / rayDirX)
	deltaDistY := math.Abs(1 / rayDirY)
	var perpWallDist float64

	//what direction to step in x or y-direction (either +1 or -1)
	var stepX int
	var stepY int

	hit := 0   //was there a wall hit?
	side := -1 //was a NS or a EW wall hit?

	//calculate step and initial sideDist
	if rayDirX < 0 {
		stepX = -1
		sideDistX = (rayPosX - float64(mapX)) * deltaDistX
	} else {
		stepX = 1
		sideDistX = (float64(mapX) + 1.0 - rayPosX) * deltaDistX
	}

	if rayDirY < 0 {
		stepY = -1
		sideDistY = (rayPosY - float64(mapY)) * deltaDistY
	} else {
		stepY = 1
		sideDistY = (float64(mapY) + 1.0 - rayPosY) * deltaDistY
	}

	//perform DDA
	for hit == 0 {
		//jump to next map square, OR in x-direction, OR in y-direction
		if sideDistX < sideDistY {
			sideDistX += deltaDistX
			mapX += stepX
			side = 0
		} else {
			sideDistY += deltaDistY
			mapY += stepY
			side = 1
		}

		//Check if ray has hit a wall
		if mapX >= 0 && mapY >= 0 && mapX < c.mapWidth && mapY < c.mapHeight {
			if grid[mapX][mapY] > 0 {
				hit = 1
			}
		} else {
			//hit grid boundary
			hit = 2
		}
	}

	//Calculate distance of perpendicular ray (oblique distance will give fisheye effect!)
	if side == 0 {
		perpWallDist = (float64(mapX) - rayPosX + (1.0-float64(stepX))/2.0) / rayDirX
	} else {
		perpWallDist = (float64(mapY) - rayPosY + (1.0-float64(stepY))/2.0) / rayDirY
	}

	//Calculate height of line to draw on screen
	lineHeight := int(float64(c.h) / perpWallDist)

	//calculate lowest and highest pixel to fill in current stripe
	drawStart := (-lineHeight/2 + c.h/2) + c.pitch + int(c.posZ/perpWallDist) - lineHeight*levelNum
	drawEnd := drawStart + lineHeight

	//--due to modern way of drawing using quads this is removed to avoid glitches at the edges--//
	// if drawStart < 0 { drawStart = 0 }
	// if drawEnd >= c.h { drawEnd = c.h - 1 }

	//texturing calculations
	texNum := -1
	if mapX >= 0 && mapY >= 0 && mapX < c.mapWidth && mapY < c.mapHeight {
		texNum = grid[mapX][mapY] - 1 //1 subtracted from it so that texture 0 can be used
	}

	//--some supid hacks to make the houses render correctly--//
	// this corrects textures on two sides of house since the textures are not symmetrical
	if side == 0 {
		if texNum == 3 {
			texNum = 4
		} else if texNum == 4 {
			texNum = 3
		}

		if texNum == 1 {
			texNum = 4
		} else if texNum == 2 {
			texNum = 3
		}
	}

	if texNum >= 0 {
		c.levels[levelNum].CurrTex[x] = c.tex.Textures[texNum]
	} else {
		c.levels[levelNum].CurrTex[x] = nil
	}

	//calculate value of wallX
	var wallX float64 //where exactly the wall was hit
	if side == 0 {
		wallX = rayPosY + perpWallDist*rayDirY
	} else {
		wallX = rayPosX + perpWallDist*rayDirX
	}
	wallX -= math.Floor(wallX)

	//x coordinate on the texture
	texX := int(wallX * float64(c.texSize))
	if side == 0 && rayDirX > 0 {
		texX = c.texSize - texX - 1
	}

	if side == 1 && rayDirY < 0 {
		texX = c.texSize - texX - 1
	}

	//--set current texture slice to be slice x--//
	_cts[x] = c.tex.slices[texX]

	//--set height of slice--//
	_sv[x].Min.Y = drawStart

	//--set draw start of slice--//
	_sv[x].Max.Y = drawEnd

	//--add a bit of tint to differentiate between walls of a corner--//
	_st[x] = &color.RGBA{255, 255, 255, 255}
	if side == 1 {
		wallDiff := 12
		_st[x].R -= byte(wallDiff)
		_st[x].G -= byte(wallDiff)
		_st[x].B -= byte(wallDiff)
	}

	//// LIGHTING ////
	//--simulates torch light, as if player was carrying a radial light--//
	var lightFalloff float64 = -100 //decrease value to make torch dimmer

	//--sun brightness, illuminates whole level--//
	var sunLight float64 = 300 //global illumination

	//--distance based dimming of light--//
	var shadowDepth float64
	shadowDepth = math.Sqrt(perpWallDist) * lightFalloff
	_st[x].R = byte(geom.ClampInt(int(float64(_st[x].R)+shadowDepth+sunLight), 0, 255))
	_st[x].G = byte(geom.ClampInt(int(float64(_st[x].G)+shadowDepth+sunLight), 0, 255))
	_st[x].B = byte(geom.ClampInt(int(float64(_st[x].B)+shadowDepth+sunLight), 0, 255))

	//SET THE ZBUFFER FOR THE SPRITE CASTING
	if levelNum == 0 {
		// for now only rendering sprites on first level
		c.zBuffer[x] = perpWallDist //perpendicular distance is used
	}

	//// FLOOR CASTING ////
	if levelNum == 0 {
		// for now only rendering floor on first level
		if drawEnd < 0 {
			drawEnd = c.h //becomes < 0 when the integer overflows
		}
		wg.Add(1)
		go func() {
			defer wg.Done()

			var floorXWall, floorYWall float64

			//4 different wall directions possible
			if side == 0 && rayDirX > 0 {
				floorXWall = float64(mapX)
				floorYWall = float64(mapY) + wallX
			} else if side == 0 && rayDirX < 0 {
				floorXWall = float64(mapX) + 1.0
				floorYWall = float64(mapY) + wallX
			} else if side == 1 && rayDirY > 0 {
				floorXWall = float64(mapX) + wallX
				floorYWall = float64(mapY)
			} else {
				floorXWall = float64(mapX) + wallX
				floorYWall = float64(mapY) + 1.0
			}

			var distWall, distPlayer, currentDist float64

			distWall = perpWallDist
			distPlayer = 0.0

			//draw the floor from drawEnd to the bottom of the screen
			for y := drawEnd + 1; y < c.h; y++ {
				currentDist = (float64(c.h) + (2.0 * c.posZ)) / (2.0*float64(y-c.pitch) - float64(c.h))

				weight := (currentDist - distPlayer) / (distWall - distPlayer)

				currentFloorX := weight*floorXWall + (1.0-weight)*rayPosX
				currentFloorY := weight*floorYWall + (1.0-weight)*rayPosY

				var floorTexX, floorTexY int
				floorTexX = int(currentFloorX*float64(c.texSize)) % c.texSize
				floorTexY = int(currentFloorY*float64(c.texSize)) % c.texSize

				//floor
				// buffer[y][x] = (texture[3][texWidth * floorTexY + floorTexX] >> 1) & 8355711;
				// the same vertical slice method cannot be used for floor rendering
				floorTexNum := 0
				floorTex := c.floorLvl.texRGBA[floorTexNum]

				//pixel := floorTex.RGBAAt(floorTexX, floorTexY)
				pxOffset := floorTex.PixOffset(floorTexX, floorTexY)
				pixel := color.RGBA{floorTex.Pix[pxOffset],
					floorTex.Pix[pxOffset+1],
					floorTex.Pix[pxOffset+2],
					floorTex.Pix[pxOffset+3]}

				// lighting
				shadowDepth = math.Sqrt(currentDist) * lightFalloff
				pixelSt := &color.RGBA{255, 255, 255, 255}
				pixelSt.R = byte(geom.ClampInt(int(float64(pixelSt.R)+shadowDepth+sunLight), 0, 255))
				pixelSt.G = byte(geom.ClampInt(int(float64(pixelSt.G)+shadowDepth+sunLight), 0, 255))
				pixelSt.B = byte(geom.ClampInt(int(float64(pixelSt.B)+shadowDepth+sunLight), 0, 255))
				pixel.R = uint8(float64(pixel.R) * float64(pixelSt.R) / 256)
				pixel.G = uint8(float64(pixel.G) * float64(pixelSt.G) / 256)
				pixel.B = uint8(float64(pixel.B) * float64(pixelSt.B) / 256)

				//c.horLvl.HorBuffer.SetRGBA(x, y, pixel)
				pxOffset = c.floorLvl.horBuffer.PixOffset(x, y)
				c.floorLvl.horBuffer.Pix[pxOffset] = pixel.R
				c.floorLvl.horBuffer.Pix[pxOffset+1] = pixel.G
				c.floorLvl.horBuffer.Pix[pxOffset+2] = pixel.B
				c.floorLvl.horBuffer.Pix[pxOffset+3] = pixel.A
			}
		}()
	}
}

func (c *Camera) castSprite(spriteOrdIndex int) {
	// the sprite
	sprite := c.sprites[c.spriteOrder[spriteOrdIndex]]

	// track whether the sprite actually needs to draw
	renderSprite := false

	//translate sprite position to relative to camera
	spriteX := sprite.Pos().X - c.pos.X
	spriteY := sprite.Pos().Y - c.pos.Y

	spriteTex := sprite.Texture()
	spriteTexRect := sprite.TextureRect()
	spriteTexWidth, spriteTexHeight := spriteTex.Size()

	//transform sprite with the inverse camera matrix
	// [ planeX   dirX ] -1                                       [ dirY      -dirX ]
	// [               ]       =  1/(planeX*dirY-dirX*planeY) *   [                 ]
	// [ planeY   dirY ]                                          [ -planeY  planeX ]

	invDet := 1.0 / (c.plane.X*c.dir.Y - c.dir.X*c.plane.Y) //required for correct matrix multiplication

	transformX := invDet * (c.dir.Y*spriteX - c.dir.X*spriteY)
	transformY := invDet * (-c.plane.Y*spriteX + c.plane.X*spriteY) //this is actually the depth inside the screen, that what Z is in 3D

	spriteScreenX := int(float64(c.w) / 2 * (1 + transformX/transformY))

	//parameters for scaling and moving the sprites
	var uDiv float64 = 1 / sprite.Scale()
	var vDiv float64 = 1 / sprite.Scale()

	var vMove float64 = -(sprite.PosZ()-0.5)*float64(c.texSize)*2 + sprite.VerticalOffset()

	vMoveScreen := int(vMove/transformY) + c.pitch + int(c.posZ/transformY)

	//calculate height of the sprite on screen
	spriteHeight := int(math.Abs(float64(c.h)/transformY) / vDiv) //using "transformY" instead of the real distance prevents fisheye
	//calculate lowest and highest pixel to fill in current stripe
	drawStartY := -spriteHeight/2 + c.h/2 + vMoveScreen
	if drawStartY < 0 {
		drawStartY = 0
	}
	drawEndY := spriteHeight/2 + c.h/2 + vMoveScreen
	if drawEndY >= c.h {
		drawEndY = c.h - 1
	}

	//calculate width of the sprite
	spriteWidth := int(math.Abs(float64(c.h)/transformY) / uDiv)
	drawStartX := -spriteWidth/2 + spriteScreenX
	drawEndX := spriteWidth/2 + spriteScreenX

	if drawStartX < 0 {
		drawStartX = 0
	}
	if drawEndX >= c.w {
		drawEndX = c.w - 1
	}

	var spriteSlices []*image.Rectangle

	//// LIGHTING ////
	//--simulates torch light, as if player was carrying a radial light--//
	var lightFalloff float64 = -100 //decrease value to make torch dimmer

	//--sun brightness, illuminates whole level--//
	var sunLight float64 = 300 //global illumination

	//loop through every vertical stripe of the sprite on screen
	for stripe := drawStartX; stripe < drawEndX; stripe++ {
		//the conditions in the if are:
		//1) it's in front of camera plane so you don't see things behind you
		//2) it's on the screen (left)
		//3) it's on the screen (right)
		//4) ZBuffer, with perpendicular distance
		if transformY > 0 && stripe > 0 && stripe < c.w && transformY < c.zBuffer[stripe] {
			var spriteLvl *level
			if !renderSprite {
				renderSprite = true
				spriteLvl = c.makeSpriteLevel(spriteOrdIndex)
				spriteSlices = makeSlices(spriteTexWidth, spriteTexHeight, spriteTexRect.Min.X, spriteTexRect.Min.Y)
			} else {
				spriteLvl = c.spriteLvls[spriteOrdIndex]
			}

			texX := int(256*(stripe-(-spriteWidth/2+spriteScreenX))*spriteTexWidth/spriteWidth) / 256

			if texX < 0 || texX >= cap(spriteSlices) {
				continue
			}

			// modify tex startY and endY based on distance
			d := (drawStartY-vMoveScreen)*256 - c.h*128 + spriteHeight*128 //256 and 128 factors to avoid floats
			texStartY := ((d * spriteTexWidth) / spriteHeight) / 256

			d = (drawEndY-1-vMoveScreen)*256 - c.h*128 + spriteHeight*128
			texEndY := ((d * spriteTexWidth) / spriteHeight) / 256

			if texStartY < 0 || texStartY >= texEndY || texEndY >= spriteTexWidth {
				continue
			}

			//--set current texture slice--//
			spriteLvl.Cts[stripe] = spriteSlices[texX]
			spriteLvl.Cts[stripe].Min.Y = spriteTexRect.Min.Y + texStartY + 1
			spriteLvl.Cts[stripe].Max.Y = spriteTexRect.Min.Y + texEndY

			spriteLvl.CurrTex[stripe] = spriteTex

			//--set draw start and height of slice--//
			spriteLvl.Sv[stripe].Min.Y = drawStartY + 1
			spriteLvl.Sv[stripe].Max.Y = drawEndY

			// distance based lighting/shading
			spriteLvl.St[stripe] = &color.RGBA{255, 255, 255, 255}

			//--distance based dimming of light--//
			shadowDepth := math.Sqrt(transformY) * lightFalloff
			spriteLvl.St[stripe].R = byte(geom.ClampInt(int(float64(spriteLvl.St[stripe].R)+shadowDepth+sunLight), 0, 255))
			spriteLvl.St[stripe].G = byte(geom.ClampInt(int(float64(spriteLvl.St[stripe].G)+shadowDepth+sunLight), 0, 255))
			spriteLvl.St[stripe].B = byte(geom.ClampInt(int(float64(spriteLvl.St[stripe].B)+shadowDepth+sunLight), 0, 255))
		}
	}

	if !renderSprite {
		c.clearSpriteLevel(spriteOrdIndex)
	}
}

// creates level slices for raycasting each level
func (c *Camera) createLevels(numLevels int) []*level {
	levelArr := make([]*level, numLevels)

	for i := 0; i < numLevels; i++ {
		levelArr[i] = new(level)
		levelArr[i].Sv = sliceView(c.w, c.h)
		levelArr[i].Cts = make([]*image.Rectangle, c.w)
		levelArr[i].St = make([]*color.RGBA, c.w)
		levelArr[i].CurrTex = make([]*ebiten.Image, c.w)
	}

	return levelArr
}

// creates floor slices for raycasting floor
func (c *Camera) createFloorLevel() *horLevel {
	horizontalLevel := new(horLevel)
	horizontalLevel.clear(c.w, c.h)
	horizontalLevel.texRGBA = []*image.RGBA{c.tex.FloorTex}
	return horizontalLevel
}

// updates sprite slice array as a level
func (c *Camera) updateSpriteLevels(spriteCapacity int) {
	if c.spriteLvls != nil {
		capacity := len(c.spriteLvls)
		if spriteCapacity <= capacity {
			// no need to grow, just need to clear it out
			c.clearAllSpriteLevel()
			return
		}

		for capacity <= spriteCapacity {
			capacity *= 2
		}

		spriteCapacity = capacity
	}
	c.spriteLvls = make([]*level, spriteCapacity)
}

func (c *Camera) makeSpriteLevel(spriteOrdIndex int) *level {
	spriteLvl := new(level)
	spriteLvl.Sv = sliceView(c.w, c.h)
	spriteLvl.Cts = make([]*image.Rectangle, c.w)
	spriteLvl.St = make([]*color.RGBA, c.w)
	spriteLvl.CurrTex = make([]*ebiten.Image, c.w)

	c.spriteLvls[spriteOrdIndex] = spriteLvl

	return spriteLvl
}

func (c *Camera) clearAllSpriteLevel() {
	for i := 0; i < len(c.spriteLvls); i++ {
		c.clearSpriteLevel(i)
	}
}

func (c *Camera) clearSpriteLevel(spriteOrdIndex int) {
	c.spriteLvls[spriteOrdIndex] = nil
}

//sort algorithm
func combSort(order []int, dist []float64, amount int) {
	gap := amount
	swapped := false
	for gap > 1 || swapped {
		//shrink factor 1.3
		gap = (gap * 10) / 13
		if gap == 9 || gap == 10 {
			gap = 11
		}
		if gap < 1 {
			gap = 1
		}

		swapped = false
		for i := 0; i < amount-gap; i++ {
			j := i + gap
			if dist[i] < dist[j] {
				// std::swap implementation for go:
				dist[i], dist[j] = dist[j], dist[i]
				order[i], order[j] = order[j], order[i]
				swapped = true
			}
		}
	}
}

// checks for valid move for the camera (not player model) from current position, returns valid (x, y) position
func (c *Camera) getValidCameraMove(moveX, moveY float64, checkAlternate bool) (float64, float64) {
	posX := c.pos.X
	posY := c.pos.Y
	newX := moveX
	newY := moveY

	if posX == newX && posY == moveY {
		return posX, posY
	}

	ix := int(newX)
	iy := int(newY)

	// prevent index out of bounds errors
	switch {
	case ix < 0 || newX < 0:
		newX = edgeDistance
		ix = 0
	case ix >= c.mapWidth:
		newX = float64(c.mapWidth) - edgeDistance
		ix = int(newX)
	}

	switch {
	case iy < 0 || newY < 0:
		newY = edgeDistance
		iy = 0
	case iy >= c.mapHeight:
		newY = float64(c.mapHeight) - edgeDistance
		iy = int(newY)
	}

	firstLevel := c.mapObj.Level(0)
	if firstLevel[ix][iy] <= 0 {
		posX = newX
		posY = newY
	}

	return posX, posY
}

// Set camera position vector
func (c *Camera) SetPosition(pos *geom.Vector2) {
	c.pos = pos
}

// Get camera position vector
func (c *Camera) GetPosition() *geom.Vector2 {
	return c.pos
}

// Set camera Z-plane position
func (c *Camera) SetPositionZ(posZ float64) {
	c.posZ = posZ
}

// Get camera Z-plane position
func (c *Camera) GetPositionZ() float64 {
	return c.posZ
}

// Set camera direction and plane vectors from given heading angle
func (c *Camera) SetHeadingAngle(heading float64) {
	cameraDir := c.GetVecForAngle(heading)
	c.dir = cameraDir
	c.plane = c.GetVecForFov(cameraDir)
}

// Set camera pitch view from given pitch angle
func (c *Camera) SetPitchAngle(pitch float64) {
	cameraPitch := geom.GetOppositeTriangleLeg(pitch, float64(c.h/2))
	c.pitch = int(cameraPitch)
}

// Move camera by move speed (does not alter player model position)
func (c *Camera) MoveCamera(mSpeed float64) {
	mx := c.pos.X + (c.dir.X * mSpeed)
	my := c.pos.Y + (c.dir.Y * mSpeed)
	c.pos.X, c.pos.Y = c.getValidCameraMove(mx, my, true)
}

// Get the angle from the dir vectors
func (c *Camera) GetAngleFromVec(dir *geom.Vector2) float64 {
	return math.Atan2(dir.Y, dir.X)
}

// Get the dir vector from angle and fov length
func (c *Camera) GetVecForAngleLength(angle, length float64) *geom.Vector2 {
	return &geom.Vector2{X: length * math.Cos(angle), Y: length * math.Sin(angle)}
}

func (c *Camera) GetVecForAngle(angle float64) *geom.Vector2 {
	return &geom.Vector2{X: c.fovDepth * math.Cos(angle), Y: c.fovDepth * math.Sin(angle)}
}

// Get the FOV from the dir+-plane vectors
func (c *Camera) GetFovFromVec(dir, plane *geom.Vector2) float64 {
	pov1 := dir.Copy().Add(plane)
	pov2 := dir.Copy().Sub(plane)
	fovAngle := math.Atan2(pov2.Y, pov2.X) - math.Atan2(pov1.Y, pov1.X)
	return fovAngle
}

// Get the plane vector from FOV based on dir vector
func (c *Camera) GetVecForFov(dir *geom.Vector2) *geom.Vector2 {
	// get the hypotenuse of half the FOV triangle to calculate the plane vec points
	angle := c.GetAngleFromVec(dir)
	length := math.Sqrt(math.Pow(dir.X, 2) + math.Pow(dir.Y, 2))
	hypotenuse := length / math.Cos(c.fovAngle/2)

	// subtract resulting vector from dir since plane vec is relative to it
	return dir.Copy().Sub(c.GetVecForAngleLength(angle+c.fovAngle/2, hypotenuse))
}

// Strafe camera by strafe speed (does not alter player model position)
func (c *Camera) StrafeCamera(sSpeed float64) {
	sx := c.pos.X + (c.plane.X * sSpeed)
	sy := c.pos.Y + (c.plane.Y * sSpeed)
	c.pos.X, c.pos.Y = c.getValidCameraMove(sx, sy, true)
}

// Rotate camera by rotate speed (does not alter player model orientation)
func (c *Camera) RotateCamera(rSpeed float64) {
	//both camera direction and camera plane must be rotated
	oldDirX := c.dir.X
	c.dir.X = (c.dir.X*math.Cos(rSpeed) - c.dir.Y*math.Sin(rSpeed))
	c.dir.Y = (oldDirX*math.Sin(rSpeed) + c.dir.Y*math.Cos(rSpeed))
	oldPlaneX := c.plane.X
	c.plane.X = (c.plane.X*math.Cos(rSpeed) - c.plane.Y*math.Sin(rSpeed))
	c.plane.Y = (oldPlaneX*math.Sin(rSpeed) + c.plane.Y*math.Cos(rSpeed))
}

// Pitch camera by pitch delta (does not alter player model orientation)
func (c *Camera) PitchCamera(pDelta int) {
	newPitch := geom.ClampInt(c.pitch+pDelta, -c.h/2, c.h/2)
	c.pitch = newPitch
}

// Stand camera position
func (c *Camera) StandCamera() {
	c.posZ = 0.0
}

// Crouch camera position
func (c *Camera) CrouchCamera() {
	c.posZ = -150.0
}

// Prone camera position
func (c *Camera) ProneCamera() {
	c.posZ = -280.0
}

// Jump camera position
func (c *Camera) JumpCamera() {
	c.posZ = 200.0
}
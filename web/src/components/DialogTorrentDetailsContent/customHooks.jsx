import { useEffect, useRef, useState } from 'react'
import axios from 'axios'

import { cacheHost, settingsHost } from 'utils/Hosts'

export const useUpdateCache = hash => {
  const [cache, setCache] = useState({})
  const componentIsMounted = useRef(true)
  const timerID = useRef(null)

  useEffect(
    () => () => {
      // this function is required to notify "updateCache" when NOT to make state update
      componentIsMounted.current = false
    },
    [],
  )

  useEffect(() => {
    if (!hash) {
      clearInterval(timerID.current)
      return undefined
    }

    const fetchCache = () => {
      if (typeof document !== 'undefined' && document.hidden) return
      axios
        .post(cacheHost(), { action: 'get', hash })
        .then(({ data }) => {
          if (componentIsMounted.current) setCache(data)
        })
        .catch(() => {
          if (componentIsMounted.current) setCache({})
        })
    }

    fetchCache()
    timerID.current = setInterval(fetchCache, 750)

    return () => clearInterval(timerID.current)
  }, [hash])

  return cache
}

export const useCreateCacheMap = cache => {
  const [cacheMap, setCacheMap] = useState([])

  useEffect(() => {
    const { PiecesCount = 0, Pieces = {}, Readers = [] } = cache || {}

    const map = []

    for (let i = 0; i < PiecesCount; i++) {
      const { Size, Length, Priority } = Pieces[i] || {}

      const newPiece = { id: i, percentage: (Size / Length) * 100 || 0, priority: Priority || 0 }

      Readers.forEach(r => {
        if (i === r.Reader) newPiece.isReader = true
        if (i >= r.Start && i < r.End) newPiece.isReaderRange = true
      })

      map.push(newPiece)
    }
    setCacheMap(map)
  }, [cache])

  return cacheMap
}

export const useGetSettings = () => {
  const [settings, setSettings] = useState()
  useEffect(() => {
    axios.post(settingsHost(), { action: 'get' }).then(({ data }) => setSettings(data))
  }, [])

  return settings
}

import { Button, Tooltip } from '@material-ui/core'
import { Check, DirectionsBoat, Error } from '@material-ui/icons'
import { createTheme, makeStyles } from '@material-ui/core/styles'
import { DataGrid } from '@material-ui/data-grid'
import { NumberParam, StringParam, useQueryParam } from 'use-query-params'
import { safeEncodeURIComponent, SafeJSONParam } from '../helpers'
import Alert from '@material-ui/lab/Alert'
import GridToolbar from '../datagrid/GridToolbar'
import PropTypes from 'prop-types'
import React, { useEffect } from 'react'

const defaultTheme = createTheme()
const useStyles = makeStyles(
  (theme) => ({
    rowPhaseSucceeded: {
      backgroundColor: theme.palette.success.light,
    },
    rowPhaseFailed: {
      backgroundColor: theme.palette.error.light,
    },
    title: {
      textAlign: 'center',
    },
  }),
  { defaultTheme }
)

function ReleasePayloadJobRuns(props) {
  const classes = useStyles()

  const columns = [
    {
      field: 'release_tag',
      headerName: 'Tag',
      hide: true,
    },
    {
      field: 'job_name',
      headerName: 'Job name',
      flex: 3,
    },

    {
      field: 'upgrades_from',
      headerName: 'Upgrades from',
      flex: 3,
    },
    {
      field: 'upgrades_to',
      headerName: 'Upgrades to',
      flex: 3,
    },
    {
      field: 'kind',
      headerName: 'Blocking',
      flex: 1.25,
      renderCell: (params) => {
        if (params.value === 'Blocking') {
          return <Check />
        } else {
          return <></>
        }
      },
    },
    {
      field: 'url',
      headerName: ' ',
      flex: 0.75,
      filterable: false,
      renderCell: (params) => {
        return (
          <Tooltip title="View in Prow">
            <Button
              style={{ justifyContent: 'center' }}
              target="_blank"
              startIcon={<DirectionsBoat />}
              href={params.value}
            />
          </Tooltip>
        )
      },
    },
  ]

  const [fetchError, setFetchError] = React.useState('')
  const [isLoaded, setLoaded] = React.useState(false)
  const [rows, setRows] = React.useState([])

  const [filterModel = props.filterModel, setFilterModel] = useQueryParam(
    'filters',
    SafeJSONParam
  )

  const [sortField = props.sortField, setSortField] = useQueryParam(
    'sortField',
    StringParam
  )
  const [sort = props.sort, setSort] = useQueryParam('sort', StringParam)

  const [pageSize = props.pageSize, setPageSize] = useQueryParam(
    'pageSize',
    NumberParam
  )

  const requestSearch = (searchValue) => {
    const currentFilters = filterModel
    currentFilters.items = currentFilters.items.filter(
      (f) => f.columnField !== 'release_tag'
    )
    currentFilters.items.push({
      id: 99,
      columnField: 'releaseTag',
      operatorValue: 'contains',
      value: searchValue,
    })
    setFilterModel(currentFilters)
  }

  const addFilters = (filter) => {
    const currentFilters = filterModel.items.filter((item) => item.value !== '')

    filter.forEach((item) => {
      if (item.value && item.value !== '') {
        currentFilters.push(item)
      }
    })
    setFilterModel({
      items: currentFilters,
      linkOperator: filterModel.linkOperator || 'and',
    })
  }

  const updateSortModel = (model) => {
    if (model.length === 0) {
      return
    }

    if (sort !== model[0].sort) {
      setSort(model[0].sort)
    }

    if (sortField !== model[0].field) {
      setSortField(model[0].field)
    }
  }

  const fetchData = () => {
    let queryString = ''
    if (filterModel && filterModel.items.length > 0) {
      queryString +=
        '&filter=' + safeEncodeURIComponent(JSON.stringify(filterModel))
    }

    if (props.release && props.release !== '') {
      queryString += '&release=' + safeEncodeURIComponent(props.release)
    }

    if (props.limit > 0) {
      queryString += '&limit=' + safeEncodeURIComponent(props.limit)
    }

    queryString += '&sortField=' + safeEncodeURIComponent(sortField)
    queryString += '&sort=' + safeEncodeURIComponent(sort)

    fetch(
      process.env.REACT_APP_API_URL +
        '/api/releases/job_runs?' +
        queryString.substring(1)
    )
      .then((response) => {
        if (response.status !== 200) {
          throw new Error('server returned ' + response.status)
        }
        return response.json()
      })
      .then((json) => {
        setRows(json)
        setLoaded(true)
      })
      .catch((error) => {
        setFetchError('Could not retrieve tags ' + error)
      })
  }

  useEffect(() => {
    fetchData()
  }, [])

  if (fetchError !== '') {
    return <Alert severity="error">{fetchError}</Alert>
  }

  if (isLoaded === false) {
    return <p>Loading...</p>
  }

  return (
    <DataGrid
      components={{ Toolbar: props.hideControls ? '' : GridToolbar }}
      rows={rows}
      columns={columns}
      autoHeight={true}
      getRowClassName={(params) => classes['rowPhase' + params.row.state]}
      disableColumnFilter={props.briefTable}
      disableColumnMenu={true}
      pageSize={pageSize}
      onPageSizeChange={(newPageSize) => setPageSize(newPageSize)}
      rowsPerPageOptions={[5, 10, 25, 50]}
      filterMode="server"
      sortingMode="server"
      sortingOrder={['desc', 'asc']}
      sortModel={[
        {
          field: sortField,
          sort: sort,
        },
      ]}
      onSortModelChange={(m) => updateSortModel(m)}
      componentsProps={{
        toolbar: {
          columns: columns,
          clearSearch: () => requestSearch(''),
          doSearch: requestSearch,
          addFilters: addFilters,
          filterModel: filterModel,
          setFilterModel: setFilterModel,
        },
      }}
    />
  )
}

ReleasePayloadJobRuns.defaultProps = {
  limit: 0,
  hideControls: false,
  pageSize: 25,
  briefTable: false,
  filterModel: {
    items: [],
  },
  sortField: 'kind',
  sort: 'asc',
}

ReleasePayloadJobRuns.propTypes = {
  briefTable: PropTypes.bool,
  hideControls: PropTypes.bool,
  limit: PropTypes.number,
  pageSize: PropTypes.number,
  filterModel: PropTypes.object,
  release: PropTypes.string,
  sort: PropTypes.string,
  sortField: PropTypes.string,
}

export default ReleasePayloadJobRuns
